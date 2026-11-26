cask "mi-guard-app" do
  # BEGIN HOMEBREW_STABLE_METADATA
  version "0.2.1"
  sha256 "348c4a01bdeac521426b7d7529ae127bcf126645fccf8d6e6de2d16c53ce0dff"
  # END HOMEBREW_STABLE_METADATA

  url "https://github.com/lonelygo/mi-guard/releases/download/v#{version}/mi-guard-app-#{version}-darwin-arm64.zip"
  name "mi-guard"
  desc "View Mihomo routes, bandwidth measurements, and selection history"
  homepage "https://github.com/lonelygo/mi-guard"

  depends_on arch: :arm64
  depends_on macos: :monterey

  app "mi-guard.app"
end
