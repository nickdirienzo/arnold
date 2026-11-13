import { readFileSync } from 'node:fs';

export function validateConfig(c) {
  const fail = (message) => { throw new Error(`Configuration: ${message}`); };
  if (!c.teamId || !c.channels || !c.users) fail('teamId, channels and users are required');
  if (c.agent !== undefined && !['codex', 'claude'].includes(c.agent)) fail('agent must be codex or claude');
  if (c.triggers?.length && !c.automation) fail('configured triggers require an automation identity');
  for (const [channel, route] of Object.entries(c.channels)) {
    if (!/^C[A-Z0-9]+$|^G[A-Z0-9]+$/.test(channel) || !route.repo) fail('channels require Slack IDs and a repository');
  }
  for (const actor of [...Object.values(c.users), ...(c.automation ? [c.automation] : [])]) {
    if (!actor.identity || !['investigation', 'implementation'].includes(actor.profile)) fail('each identity needs an allowed profile');
  }
  if (c.automation && c.automation.profile !== 'investigation') fail('automation must use investigation');
  if (c.automation?.recipientUserId && !/^[UW][A-Z0-9]+$/.test(c.automation.recipientUserId)) fail('automation recipientUserId must be a Slack member ID');
  for (const t of c.triggers ?? []) {
    if (!t.id || !c.channels[t.channel] || (!t.botId && !t.userId) || (t.botId && t.userId)) fail('triggers require id, configured channel and exactly one source botId or userId');
    if (typeof t.contains !== 'string' || !t.contains.trim()) fail('triggers require nonempty contains');
  }
  if (new Set((c.triggers ?? []).map(t => t.id)).size !== (c.triggers ?? []).length) fail('trigger IDs must be unique');
  return c;
}

export const loadConfig = path => validateConfig(JSON.parse(readFileSync(path, 'utf8')));
