// Command mihomo-guard maintains bandwidth-aware managed selectors for Mihomo.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
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
  check         Verify wiring and show live availability, current member, latency, bandwidth and tenure
  status        Show current groups and accounted usage (--probe for fresh checks)
  history       Print a read-only JSON snapshot of recorded usage, samples and group selections
  bench         Measure managed groups, or use -g GROUP, --node NAME, -f REGEXP
                --source FILE|URL measures an ad-hoc provider using the installed core
                --apply selects or keeps a measured member of a manual selector
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
	bytes, bytesPerStream int64
	streams               int
	group, filter         string
	nodes, sources        stringsFlag
	apply                 bool
}

func (o *benchOptions) configureDownloads(speed *config.Speed, flags *flag.FlagSet) error {
	set := make(map[string]bool)
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["bytes"] && set["bytes-per-stream"] {
		return errors.New("choose either --bytes (per node) or --bytes-per-stream")
	}
	if set["streams"] {
		if o.streams < 1 || o.streams > model.MaxDownloadStreams {
			return fmt.Errorf("--streams must be from 1 to %d", model.MaxDownloadStreams)
		}
		speed.BenchStreams = o.streams
	}
	if set["bytes"] {
		if o.bytes < int64(speed.BenchStreams) {
			return errors.New("--bytes must provide at least one byte per stream")
		}
		return nil
	}
	if set["bytes-per-stream"] {
		speed.BenchBytesPerStream = o.bytesPerStream
	}
	var err error
	o.bytes, err = speed.BenchTotalBytes()
	if err != nil && set["bytes-per-stream"] {
		return fmt.Errorf("--bytes-per-stream: %w", err)
	}
	o.bytesPerStream = speed.BenchBytesPerStream
	return err
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
		fs.Int64Var(&opt.bytes, "bytes", 0, "total download bytes per node, shared across streams (overrides the per-stream cap)")
		fs.Int64Var(&opt.bytesPerStream, "bytes-per-stream", 0, "download byte cap for each stream (from config, initially 4 MiB)")
		fs.IntVar(&opt.streams, "streams", 0, "parallel downloads per node (1–8, from config, initially 4)")
		fs.StringVar(&opt.group, "g", "", "group or dialer chain; exact name or unique substring")
		fs.StringVar(&opt.group, "group", "", "group or dialer chain; exact name or unique substring")
		fs.StringVar(&opt.filter, "f", "", "node name regexp")
		fs.StringVar(&opt.filter, "filter", "", "node name regexp")
		fs.Var(&opt.nodes, "node", "exact node name (repeatable)")
		fs.Var(&opt.sources, "source", "local or HTTP(S) provider YAML (repeatable)")
		fs.BoolVar(&opt.apply, "apply", false, "select or keep the best eligible member; automatic groups are measured only")
	case "status":
		fs.BoolVar(&live, "probe", false, "also probe managed live-path liveness and configured geography")
	case "init", "check", "history", "daemon", "notify-setup":
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
	if cmd == "history" {
		return writeDashboardHistory(os.Stdout, path)
	}
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	if cmd == "bench" {
		if err := opt.configureDownloads(&c.Speed, fs); err != nil {
			return err
		}
	}
	if cmd == "notify-setup" {
		return notify.Authorize(ctx, c.Notifications.Helper)
	}
	if cmd == "bench" {
		fmt.Fprintln(os.Stderr, "正在读取测速候选…")
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
	sender := notify.New(c.Notifications, path, log)
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
	if cmd == "daemon" || cmd == "check" {
		if err = f.CheckManaged(c.Groups); err != nil {
			return err
		}
	}
	switch cmd {
	case "check":
		return check(ctx, r)
	case "daemon":
		if len(c.Groups) == 0 {
			return errors.New("daemon needs at least one managed group")
		}
		return r.Store.WithLock(ctx, "daemon", func() error { return r.Daemon(ctx) })
	case "bench":
		return bench(ctx, r, f, opt)
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
		direct := g.Direct
		if mapped := r.Cfg.Selection.FollowGroups[g.Name]; mapped != "" {
			direct = mapped
		}
		for _, name := range []string{g.Name, direct} {
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

func bench(ctx context.Context, r *guard.Runner, wiring *config.CoreFile, o benchOptions) error {
	re, err := regexp.Compile(o.filter)
	if err != nil {
		return fmt.Errorf("--filter: %w", err)
	}
	var queries []string
	switch {
	case o.group != "":
		queries = []string{o.group}
	case len(o.nodes) > 0 || o.filter != "" || len(o.sources) > 0:
		if o.apply {
			return errors.New("use -g GROUP with --apply when filtering nodes")
		}
		queries = []string{r.Cfg.Mihomo.TestGroup}
	default:
		for _, g := range r.Cfg.Groups {
			queries = append(queries, g.Name)
		}
	}
	if len(queries) == 0 {
		return errors.New("choose a group, --filter or --node")
	}
	var targets []guard.BenchTarget
	members := make(map[string][]string)
	var nodes []string
	for _, query := range queries {
		target, err := r.ResolveBenchTarget(ctx, query)
		if err != nil {
			return err
		}
		name := target.Group.Name
		if o.group == "" && name != query {
			return fmt.Errorf("configured selector %s was not found by its exact name", model.QuoteName(query))
		}
		if o.apply {
			if name == r.Cfg.Mihomo.TestGroup {
				return errors.New("the dedicated test selector cannot be an --apply target")
			}
			if _, managed := r.Managed(name); managed && target.Type == "Selector" {
				if err := wiring.CheckManaged([]config.Group{target.Group}); err != nil {
					return err
				}
			}
		}
		if target.Group.Chain != "" {
			fmt.Fprintf(os.Stderr, "%s → 前置组 %s\n", displayText(target.Group.Chain), displayText(name))
		} else if o.group != "" {
			fmt.Fprintf(os.Stderr, "测速组 %s (%s)\n", displayText(name), displayText(target.Type))
		}
		names, err := r.BenchCandidates(ctx, target, o.apply)
		if err != nil {
			return err
		}
		for _, n := range names {
			if re.MatchString(n) && (len(o.nodes) == 0 || slices.Contains(o.nodes, n)) {
				members[name] = append(members[name], n)
				nodes = append(nodes, n)
			}
		}
		targets = append(targets, target)
	}
	for _, exact := range o.nodes {
		if !slices.Contains(nodes, exact) {
			return fmt.Errorf("node %s is missing or filtered out", model.QuoteName(exact))
		}
	}
	if len(nodes) == 0 {
		return errors.New("no matching physical candidates")
	}
	var past history.Snapshot
	if o.apply {
		past, err = r.Store.Snapshot(ctx)
		if err != nil {
			return err
		}
	}
	endpoint, err := url.Parse(r.Cfg.Speed.URL)
	if err != nil {
		return err
	}
	capacity := fmt.Sprintf("每节点共 %s，按流均分", byteCount(o.bytes))
	if o.bytesPerStream > 0 {
		capacity = fmt.Sprintf("每流上限 %s，每节点共 %s", byteCount(o.bytesPerStream), byteCount(o.bytes))
	}
	fmt.Fprintf(os.Stderr, "下载目标 %s · 每节点 %d 流 · %s · 超时 %s\n", displayText(endpoint.Host), r.Cfg.Speed.BenchStreams, capacity, r.Cfg.Speed.Timeout)
	progress := newBenchProgress(os.Stderr, stderrColumns())
	results, scanErr := r.ScanWithProgress(ctx, nodes, "bench", o.bytes, progress.update)
	progress.close()
	printResults(os.Stdout, results)
	if scanErr != nil {
		return scanErr
	}
	if o.apply {
		followed := make(map[string]bool)
		for _, target := range targets {
			name := target.Group.Name
			if followed[name] {
				continue
			}
			var measured []model.Sample
			for _, s := range results {
				if slices.Contains(members[name], s.Node) {
					measured = append(measured, s)
				}
			}
			ranked := policy.Rank(target.Current, measured, past.Samples, r.Cfg.Selection.NearBestPct)
			outcomes, err := r.ApplyBenchTarget(ctx, target, ranked)
			for i, outcome := range outcomes {
				if i > 0 {
					fmt.Print("随动 · ")
					if outcome.Action == "switched" || outcome.Action == "kept" {
						followed[outcome.Group] = true
					}
				}
				printSelection(outcome)
			}
			if err != nil {
				return err
			}
		}
	}
	if !slices.ContainsFunc(results, func(s model.Sample) bool { return s.Mbps > 0 && s.Error == "" }) {
		return errors.New("all measured candidates failed")
	}
	return nil
}

func printSelection(result guard.SelectionResult) {
	name := displayText(result.Group)
	reason := displayText(result.Reason)
	note := ""
	if reason != "" {
		note = "（" + reason + "）"
	}
	switch result.Action {
	case "switched":
		fmt.Printf("切换 %s：%s → %s%s\n", name, displayText(result.Before), displayText(result.After), note)
	case "kept":
		fmt.Printf("保持 %s：%s%s\n", name, displayText(result.After), note)
	case "skipped":
		fmt.Printf("跳过 %s：%s\n", name, reason)
	}
}
