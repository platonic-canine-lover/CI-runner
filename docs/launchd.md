# Running as a launchd agent (macOS)

`~/Library/LaunchAgents/com.ci-runner.plist`:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.ci-runner</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/ci-runner</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <!-- PATH matters: the build steps need node, npm and git, and a
         launchd agent does not inherit a login shell's PATH. -->
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    <key>CI_REPO</key>
    <string>owner/name</string>
    <key>CI_STEPS</key>
    <string>/Users/you/.ci-runner/steps.json</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>/Users/you/.ci-runner/ci-runner.log</string>
  <key>StandardErrorPath</key>
  <string>/Users/you/.ci-runner/ci-runner.err</string>
</dict>
</plist>
```

Load it:

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.ci-runner.plist
launchctl list | grep ci-runner
curl -s 127.0.0.1:3101/status
```

Unload it:

```sh
launchctl bootout gui/$(id -u)/com.ci-runner
```

## Token

If `CI_GITHUB_TOKEN` is not set the service shells out to `gh auth token`.
That works under launchd only if `gh` is on the `PATH` set above, and if
the token is in a keyring the agent can read. Setting `CI_GITHUB_TOKEN`
explicitly is the more predictable option for an always-on service.
