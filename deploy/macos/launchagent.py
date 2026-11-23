"""Render a user LaunchAgent with literal absolute paths (launchd does not expand ~)."""
import plistlib
import sys
from pathlib import Path

binary, config, log = (str(Path(arg).expanduser().resolve()) for arg in sys.argv[1:])
plistlib.dump({
    "Label": "com.lonelygo.mi-guard",
    "ProgramArguments": [binary, "daemon", "-c", config],
    "RunAtLoad": True,
    "KeepAlive": True,
    "ThrottleInterval": 30,
    "ProcessType": "Background",
    "StandardOutPath": log,
    "StandardErrorPath": log,
}, sys.stdout.buffer)
