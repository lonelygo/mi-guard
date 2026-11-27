class MiGuard < Formula
  desc "Bandwidth-aware Mihomo route guard for macOS"
  homepage "https://github.com/lonelygo/mi-guard"
  license "GPL-3.0-only"

  # BEGIN HOMEBREW_STABLE_METADATA
  stable do
    url "https://github.com/lonelygo/mi-guard/releases/download/v0.2.2/mi-guard-0.2.2-darwin-arm64.tar.gz"
    version "0.2.2"
    sha256 "f668c28e762107f83edc94398ff8436eed29ef0129d3d46738ea02aacbd4dbd7"
    depends_on arch: :arm64
  end
  # END HOMEBREW_STABLE_METADATA

  head do
    url "https://github.com/lonelygo/mi-guard.git", branch: "main"

    depends_on "go" => :build
    uses_from_macos "swift" => :build
  end

  depends_on macos: :monterey

  deny_network_access!

  def fetch
    return unless build.head?

    system "go", "mod", "download"
  end

  def install
    if build.head?
      system "make", "cli", "app"

      bin.install "build/mihomo-guard"
      libexec.install "build/mi-guard.app"
    else
      bin.install "bin/mihomo-guard"
      libexec.install "libexec/mi-guard.app"
    end

    pkgshare.install "share/mi-guard/mihomo-overlay.yaml"
    if !build.head? && version == Version.new("0.1.0")
      # This archive contains its default YAML only in the executable.
      reference = pkgshare/"mihomo-guard-default.yaml"
      system bin/"mihomo-guard", "init", "-c", reference
      reference.chmod 0644
    else
      pkgshare.install "share/mi-guard/mihomo-guard-default.yaml"
    end
  end

  def caveats
    <<~EOS
      Run `mihomo-guard init` on first use, or reuse your existing
      ~/.config/mihomo-guard/mihomo-guard.yaml.
      Set mihomo.config_path and group names to match your Mihomo setup.
      Set mihomo.stable_binary when using `bench --source`.
      Read the defaults and examples for this version in
        #{opt_pkgshare}/mihomo-guard-default.yaml

      Merge the proxy-groups and listeners in the following file into your
      active Mihomo configuration, then reload Mihomo.
        #{opt_pkgshare}/mihomo-overlay.yaml

      Request notification permission in your desktop login session.
        mihomo-guard notify-setup

      Verify that the running Mihomo core has loaded your listeners.
        mihomo-guard check

      Start the service as your login user. Logs go to ~/Library/Logs/mihomo-guard.log.
        brew services start lonelygo/mi-guard/mi-guard

      To install the dashboard in Applications, choose either option.
        1. Homebrew
           brew install --cask lonelygo/mi-guard/mi-guard-app
        2. GitHub Releases
           https://github.com/lonelygo/mi-guard/releases/download/v#{stable.version}/mi-guard-app-#{stable.version}-darwin-arm64.zip
           Unzip the download and drag mi-guard.app into Applications.
    EOS
  end

  service do
    run [opt_bin/"mihomo-guard", "daemon", "-c", "#{Dir.home}/.config/mihomo-guard/mihomo-guard.yaml"]
    environment_variables HOME: Dir.home, PATH: std_service_path_env
    working_dir Dir.home
    keep_alive true
    throttle_interval 30
    process_type :background
    log_path "#{Dir.home}/Library/Logs/mihomo-guard.log"
    error_log_path "#{Dir.home}/Library/Logs/mihomo-guard.log"
  end

  test do
    assert_match "mihomo-guard <command>", shell_output("#{bin}/mihomo-guard --help")

    config = testpath/"mihomo-guard.yaml"
    system bin/"mihomo-guard", "init", "-c", config
    assert_equal config.read, (pkgshare/"mihomo-guard-default.yaml").read

    overlay = pkgshare/"mihomo-overlay.yaml"
    assert_match "mi-guard-bench", overlay.read

    app = libexec/"mi-guard.app"
    assert_predicate app/"Contents/MacOS/mi-guard-notify", :executable?
    assert_path_exists app/"Contents/Resources/AppIcon.icns"
    system "/usr/bin/codesign", "--verify", "--deep", "--strict", app
  end
end
