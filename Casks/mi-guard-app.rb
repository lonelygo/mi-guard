cask "mi-guard-app" do
  # BEGIN HOMEBREW_STABLE_METADATA
  version "0.2.2"
  sha256 "7cceca87349492f1c7e75f777362f737360a84c394256140552ecc507d6c216f"
  # END HOMEBREW_STABLE_METADATA

  url "https://github.com/lonelygo/mi-guard/releases/download/v#{version}/mi-guard-app-#{version}-darwin-arm64.zip"
  name "mi-guard"
  desc "View Mihomo routes, bandwidth measurements, and selection history"
  homepage "https://github.com/lonelygo/mi-guard"

  depends_on arch: :arm64
  depends_on macos: :monterey

  app "mi-guard.app"
end
