import { SocketModeClient } from '@slack/socket-mode';
import { WebClient } from '@slack/web-api';
import { mkdirSync, chmodSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { setTimeout } from 'node:timers/promises';
import { loadConfig } from './config.js';
import { Store } from './store.js';
import { Slack } from './slack.js';
import { Paddock } from './paddock.js';
import { Arnold } from './arnold.js';

process.umask(0o077);
const config = loadConfig(process.env.ARNOLD_CONFIG ?? 'config/local.json');
const paddock = new Paddock(process.env.PADDOCK_BIN);
await paddock.check(config);
if (!process.env.SLACK_BOT_TOKEN || !process.env.SLACK_APP_TOKEN) throw new Error('Set SLACK_BOT_TOKEN and SLACK_APP_TOKEN');
const web = new WebClient(process.env.SLACK_BOT_TOKEN);
const auth = await web.auth.test();
if (auth.team_id !== config.teamId || !auth.user_id || !auth.bot_id) throw new Error('Slack token does not match the configured workspace/bot');
const path = resolve(process.env.ARNOLD_STATE ?? 'state/arnold.sqlite');
mkdirSync(dirname(path), { recursive: true, mode: 0o700 });
const store = new Store(path);
chmodSync(path, 0o600);
const arnold = new Arnold({ config, store, paddock, slack: new Slack(web), botUserId: auth.user_id, botId: auth.bot_id });
const socket = new SocketModeClient({ appToken: process.env.SLACK_APP_TOKEN });
let stopping = false;
socket.on('slack_event', async ({ body, ack }) => {
  try {
    const receipt = arnold.receive(body);
    await ack();
    if (receipt) await arnold.syncReaction(receipt);
  }
  catch (error) { console.error('Intake failed; leaving envelope unacknowledged', error); }
});
socket.on('error', error => console.error('Socket Mode error', error));
for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, () => { stopping = true; });
await socket.start();
console.log('Arnold listening in Socket Mode');
try {
  while (!stopping) {
    await arnold.drain();
    await arnold.poll();
    await setTimeout(5000);
  }
} finally { await socket.disconnect(); store.close(); }
