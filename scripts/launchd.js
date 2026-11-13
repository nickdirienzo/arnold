import { resolve } from 'node:path';
import { homedir } from 'node:os';

// Render for review; installing the plist is an operator action.
const root = resolve(import.meta.dirname, '..');
const xml = s => s.replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');
const node = xml(process.execPath);
const dir = xml(root);
const executablePath = xml(`${homedir()}/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin`);
console.log(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>dev.arnold.slack</string>
  <key>ProgramArguments</key><array>
    <string>${node}</string><string>--env-file=${dir}/.env</string><string>${dir}/src/main.js</string>
  </array>
  <key>WorkingDirectory</key><string>${dir}</string>
  <key>EnvironmentVariables</key><dict><key>PATH</key><string>${executablePath}</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>30</integer>
  <key>StandardOutPath</key><string>${dir}/state/stdout.log</string>
  <key>StandardErrorPath</key><string>${dir}/state/stderr.log</string>
</dict></plist>`);
