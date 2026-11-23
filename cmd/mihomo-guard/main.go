// Command mihomo-guard maintains bandwidth-aware managed selectors for Mihomo.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/lonelygo/mi-guard/internal/adhoc"
	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/guard"
	"github.com/lonelygo/mi-guard/internal/history"
	"github.com/lonelygo/mi-guard/internal/mihomo"
	"github.com/lonelygo/mi-guard/internal/model"
	"github.com/lonelygo/mi-guard/internal/notify"
	"github.com/lonelygo/mi-guard/internal/policy"
)

const usage = `mihomo-guard <command> [-c guard.yaml]

  -v, --version  Print the mihomo-guard version
  -h, --help     Show this help (use <command> --help for its options)

  init          Write a new guard configuration
  check         Verify controller, isolated measurement route and managed groups
  status        Show current groups and accounted usage (--probe for fresh checks)
  bench         Measure managed groups, or use -g GROUP, --node NAME, -f REGEXP
                --source FILE|URL measures an ad-hoc provider using the installed core
                --apply explicitly applies results to managed selectors
  daemon        Run heartbeats, capped speed checks and failover
  notify-setup  Request macOS notification permission for the bundled helper
`

// Release packages set this from the same version used for the archive and app.
var releaseVersion string

func buildVersion() string {
	if releaseVersion != "" {
		return releaseVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return "dev"
}

type stringsFlag []string

func (f *stringsFlag) String() string { return strings.Join(*f, ", ") }
func (f *stringsFlag) Set(s string) error {
	if strings.TrimSpace(s) == "" {
		return errors.New("empty value")
	}
	*f = append(*f, s)
	return nil
}

type benchOptions struct {
	bytes          int64
	streams        int
	group, filter  string
	nodes, sources stringsFlag
	apply          bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil && !cleanInterrupt(err) {
		fmt.Fprintln(os.Stderr, "mihomo-guard:", err)
		os.Exit(1)
	}
}

// A clean Ctrl+C exits successfully; a failed selector restore or budget
// settlement joined to that cancellation must still be reported.
func cleanInterrupt(err error) bool {
	if multi, ok := err.(interface{ Unwrap() []error }); ok {
		for _, cause := range multi.Unwrap() {
			if !cleanInterrupt(cause) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return cleanInterrupt(cause)
	}
	return err == context.Canceled
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return nil
	}
	cmd := args[0]
	if cmd == "-v" || cmd == "--version" {
		fmt.Println("mihomo-guard", buildVersion())
		return nil
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "Usage: mihomo-guard %s [options]\n", cmd)
		fs.PrintDefaults()
	}
	var help bool
	fs.BoolVar(&help, "h", false, "show this command's help")
	fs.BoolVar(&help, "help", false, "show this command's help")
	path := config.DefaultPath()
	fs.StringVar(&path, "c", path, "guard YAML file")
	fs.StringVar(&path, "config", path, "guard YAML file")
	var opt benchOptions
	var live bool
	switch cmd {
	case "bench":
		fs.Int64Var(&opt.bytes, "bytes", 0, "total response-byte cap per node (default from config)")
		fs.IntVar(&opt.streams, "streams", 0, "parallel downloads per node (1–8, share --bytes)")
		fs.StringVar(&opt.group, "g", "", "group name")
		fs.StringVar(&opt.group, "group", "", "group name")
		fs.StringVar(&opt.filter, "f", "", "node name regexp")
		fs.StringVar(&opt.filter, "filter", "", "node name regexp")
		fs.Var(&opt.nodes, "node", "exact node name (repeatable)")
		fs.Var(&opt.sources, "source", "local or HTTP(S) provider YAML (repeatable)")
		fs.BoolVar(&opt.apply, "apply", false, "apply the result to managed groups")
	case "status":
		fs.BoolVar(&live, "probe", false, "also probe managed live-path liveness and configured geography")
	case "init", "check", "daemon", "notify-setup":
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if help {
		fs.SetOutput(os.Stdout)
		fs.Usage()
		return nil
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments %q", fs.Args())
	}
	if cmd == "init" {
		if err := config.WriteExample(path); err != nil {
			return err
		}
		fmt.Println(path)
		return nil
	}
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	if cmd == "bench" {
		var invalid error
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "bytes":
				if opt.bytes <= 0 {
					invalid = errors.New("--bytes must be positive")
				} else {
					c.Speed.SweepBytes = opt.bytes
				}
			case "streams":
				if opt.streams < 1 || opt.streams > 8 {
					invalid = errors.New("--streams must be from 1 to 8")
				} else {
					c.Speed.Streams = opt.streams
				}
			}
		})
		if invalid != nil {
			return invalid
		}
	}
	if cmd == "notify-setup" {
		return notify.Authorize(ctx, c.Notifications.Helper)
	}
	if len(opt.sources) > 0 {
		if opt.apply || opt.group != "" {
			return errors.New("--source measures provider nodes; use --filter or --node to restrict the list")
		}
		var stop func()
		c, stop, err = adhoc.Start(ctx, c, opt.sources)
		if err != nil {
			return err
		}
		defer stop()
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	sender := notify.New(c.Notifications, log)
	defer sender.Close()
	r := &guard.Runner{Cfg: c, Core: mihomo.New(c.Mihomo.Controller, c.Mihomo.Secret), Store: history.New(c.History.File, c.History.SamplesPerNode, c.Speed.BillingDay), Notify: sender.Send, Log: log}
	if cmd == "status" {
		return status(ctx, r, live)
	}
	f, err := config.ReadCoreFile(c.Mihomo.ConfigPath)
	if err != nil {
		return err
	}
	if err = f.CheckBench(c.Mihomo); err != nil {
		return err
	}
	if cmd == "daemon" || cmd == "check" || opt.apply {
		groups := c.Groups
		if opt.apply && opt.group != "" {
			g, known := r.Managed(opt.group)
			if !known {
				return fmt.Errorf("--apply requires a managed group, got %s", model.QuoteName(opt.group))
			}
			groups = []config.Group{g}
		}
		if err = f.CheckManaged(groups); err != nil {
			return err
		}
	}
	switch cmd {
	case "check":
		v, err := r.Core.Version(ctx)
		if err != nil {
			return err
		}
		p, err := r.Core.Proxy(ctx, c.Mihomo.TestGroup)
		if err != nil {
			return err
		}
		if p.Type != "Selector" || len(p.All) == 0 {
			return errors.New("live core is missing the configured test selector or its candidates")
		}
		if err := r.CheckIngress(ctx, c.Mihomo.TestProxy, c.Speed.URL, c.Mihomo.TestGroup, p.Now); err != nil {
			return fmt.Errorf("cannot verify live test listener %s for current member %s: %w", c.Mihomo.TestProxy, model.QuoteName(p.Now), err)
		}
		for _, g := range c.Groups {
			nodes, e := checkManagedGroup(ctx, r, g)
			if e != nil {
				return e
			}
			if g.Mode == config.GroupModeSelector {
				fmt.Printf("%s: %d candidates\n", g.Name, len(nodes))
			} else {
				fmt.Printf("%s via %s: %d candidates\n", g.ProbeTarget(), g.Name, len(nodes))
			}
		}
		fmt.Printf("ready: Mihomo %s, %s (%d members)\n", v, p.Name, len(p.All))
		return nil
	case "daemon":
		if len(c.Groups) == 0 {
			return errors.New("daemon needs at least one managed group")
		}
		return r.Store.WithLock(ctx, "daemon", func() error { return r.Daemon(ctx) })
	case "bench":
		return bench(ctx, r, opt)
	}
	return nil
}

func status(ctx context.Context, r *guard.Runner, live bool) error {
	v, err := r.Core.Version(ctx)
	if err != nil {
		return err
	}
	all, err := r.Core.Proxies(ctx)
	if err != nil {
		return err
	}
	s, err := r.Store.Snapshot(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Mihomo %s | budget %d / %d bytes | %d bandwidth samples\n", v, s.UsedBytes, r.Cfg.Speed.MonthlyBytes, len(s.Samples))
	out := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(out, "GROUP\tCURRENT\tMEMBERS\tPROBE")
	for _, g := range r.Cfg.Groups {
		for _, name := range []string{g.Name, g.Direct} {
			if name == "" {
				continue
			}
			p, ok := all[name]
			if !ok {
				fmt.Fprintf(out, "%s\t(missing)\t0\t-\n", name)
				continue
			}
			label := "-"
			if name == g.Name {
				label = g.ProbeTarget()
			}
			fmt.Fprintf(out, "%s\t%s\t%d\t%s\n", name, p.Now, len(p.All), label)
		}
	}
	if err := out.Flush(); err != nil {
		return err
	}
	if live {
		for _, g := range r.Cfg.Groups {
			target := g.ProbeTarget()
			ms, e := r.Core.Delay(ctx, target, r.Cfg.Speed.LivenessURL, r.Cfg.Schedule.HeartbeatTimeout)
			if e != nil {
				fmt.Printf("%s heartbeat: %v\n", target, e)
			} else {
				fmt.Printf("%s heartbeat: %d ms\n", target, ms)
			}
			if g.GeoProxy != "" {
				geo, e := r.ExitStatus(ctx, g)
				fmt.Printf("%s geo_ok=%t domain=%s ip=%s colo=%s loc=%s", target, geo.OK, geo.Domain, geo.IP, geo.Colo, geo.Loc)
				if e != nil {
					fmt.Printf(" error=%v", e)
				}
				fmt.Println()
			}
		}
	}
	return nil
}

func checkManagedGroup(ctx context.Context, r *guard.Runner, g config.Group) ([]string, error) {
	primary, err := r.Core.Proxy(ctx, g.Name)
	if err != nil {
		return nil, err
	}
	if primary.Type != "Selector" {
		return nil, fmt.Errorf("invalid live target %s: got %s, want Selector", g.Name, primary.Type)
	}
	if g.Mode != config.GroupModeSelector {
		chain, chainErr := r.Core.Proxy(ctx, g.Chain)
		if chainErr != nil {
			return nil, chainErr
		}
		if chain.Name != g.Chain || chain.Dialer != g.Name {
			return nil, fmt.Errorf("invalid live target %s", g.Name)
		}
	}
	target := g.ProbeTarget()
	if g.GeoProxy != "" {
		if err := r.CheckIngress(ctx, g.GeoProxy, r.Cfg.Speed.GeoURL, target); err != nil {
			return nil, fmt.Errorf("cannot verify live geo listener %s for %s: %w", g.GeoProxy, model.QuoteName(target), err)
		}
	}
	return r.Candidates(ctx, g.Name)
}

func bench(ctx context.Context, r *guard.Runner, o benchOptions) error {
	re, err := regexp.Compile(o.filter)
	if err != nil {
		return fmt.Errorf("--filter: %w", err)
	}
	var groups []string
	switch {
	case o.group != "":
		groups = []string{o.group}
	case len(o.nodes) > 0 || o.filter != "" || len(o.sources) > 0:
		groups = []string{r.Cfg.Mihomo.TestGroup}
	default:
		for _, g := range r.Cfg.Groups {
			groups = append(groups, g.Name)
		}
	}
	if len(groups) == 0 {
		return errors.New("choose a group, --filter or --node")
	}
	members := map[string][]string{}
	previous := map[string]string{}
	var nodes []string
	for _, group := range groups {
		if o.apply {
			if _, ok := r.Managed(group); !ok {
				return fmt.Errorf("--apply requires a managed selector, got %s", model.QuoteName(group))
			}
			p, e := r.Core.Proxy(ctx, group)
			if e != nil {
				return e
			}
			previous[group] = p.Now
		}
		names, e := r.Candidates(ctx, group)
		if e != nil {
			return e
		}
		for _, n := range names {
			if !re.MatchString(n) || (len(o.nodes) > 0 && !slices.Contains(o.nodes, n)) {
				continue
			}
			members[group] = append(members[group], n)
			nodes = append(nodes, n)
		}
	}
	for _, exact := range o.nodes {
		if !slices.Contains(nodes, exact) {
			return fmt.Errorf("node %s is missing or filtered out", model.QuoteName(exact))
		}
	}
	if len(nodes) == 0 {
		return errors.New("no matching physical candidates")
	}
	past, err := r.Store.Snapshot(ctx)
	if err != nil {
		return err
	}
	results, scanErr := r.Scan(ctx, nodes, "bench", r.Cfg.Speed.SweepBytes)
	printResults(results)
	if scanErr != nil {
		return scanErr
	}
	count := 0
	for _, s := range results {
		if s.Mbps > 0 && s.Error == "" {
			count++
		}
	}
	if count == 0 {
		return errors.New("all measured candidates failed")
	}
	if o.apply {
		for _, name := range groups {
			g, _ := r.Managed(name)
			var measured []model.Sample
			for _, s := range results {
				if slices.Contains(members[name], s.Node) {
					measured = append(measured, s)
				}
			}
			ranked := policy.Rank(previous[name], measured, past.Samples, r.Cfg.Selection.NearBestPct)
			if len(ranked) == 0 {
				return fmt.Errorf("no working candidate for %s", model.QuoteName(name))
			}
			selected, e := r.Apply(ctx, g, previous[name], ranked)
			if e != nil {
				return e
			}
			fmt.Printf("selected %s -> %s\n", name, selected)
		}
	}
	return nil
}

func printResults(in []model.Sample) {
	rows := slices.Clone(in)
	slices.SortStableFunc(rows, func(a, b model.Sample) int {
		if a.Mbps > b.Mbps {
			return -1
		}
		if a.Mbps < b.Mbps {
			return 1
		}
		return strings.Compare(a.Node, b.Node)
	})
	out := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(out, "NODE\tDOWNLOAD Mbps\tPROBE ms\tFAILURES\tBYTES\tRESULT")
	for _, s := range rows {
		result := s.Error
		if result == "" {
			result = "ok"
		}
		fmt.Fprintf(out, "%s\t%.3f\t%d\t%d/%d\t%d\t%s\n", s.Node, s.Mbps, s.LatencyMS, s.Failures, s.Attempts, s.Bytes, result)
	}
	out.Flush()
}
