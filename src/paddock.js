import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const execute = promisify(execFile);

export class Paddock {
  constructor(binary = 'paddock') { this.binary = binary; }
  async call(args, maxBuffer = 4 * 1024 * 1024) {
    // Never forward Slack tokens or parent-task settings to agent execution.
    const env = Object.fromEntries(Object.entries(process.env).filter(([k]) =>
      ['PATH', 'HOME', 'USER', 'LOGNAME', 'TMPDIR', 'LANG', 'PADDOCK_HOME', 'XDG_STATE_HOME', 'REX_SERVER'].includes(k)));
    const { stdout } = await execute(this.binary, args, { env, timeout: 180_000, maxBuffer });
    return JSON.parse(stdout);
  }
  async check(config) {
    // A legacy binary cannot silently fall back to anonymous/default-identity work.
    const identities = await this.call(['identity', 'list', '--json']);
    if (config) for (const actor of [...Object.values(config.users), ...(config.automation ? [config.automation] : [])]) {
      const identity = identities[actor.identity];
      if (!identity || identity.disabled || identity.mode !== 'app' ||
          (identity.profiles?.length && !identity.profiles.includes(actor.profile)))
        throw new Error(`Paddock identity ${actor.identity} must be an active GitHub App allowing ${actor.profile}`);
    }
  }
  async run({ prompt, repo, actor, identity, profile, externalId, context, agent = 'codex' }) {
    const dir = await mkdtemp(join(tmpdir(), 'arnold-'));
    try {
      await writeFile(join(dir, 'prompt'), prompt, { mode: 0o600 });
      await writeFile(join(dir, 'context'), JSON.stringify(context), { mode: 0o600 });
      const task = await this.call(['run', '--prompt-file', join(dir, 'prompt'), '--repo', repo,
        '--runtime', 'openshell', '--agent', agent, '--identity', identity, '--profile', profile,
        '--requester', actor, '--requester-source', 'arnold-slack', '--external-id', externalId,
        '--context-file', join(dir, 'context'), '--json', '--detach']);
      this.verify(task, identity, profile, agent);
      return task;
    } finally { await rm(dir, { recursive: true, force: true }); }
  }
  verify(task, identity, profile, agent) {
    if (!task.id || task.runtime !== 'openshell' || task.provenance?.identity !== identity || task.provenance?.profile !== profile ||
        (agent && task.session?.agent !== agent))
      throw new Error('Paddock returned an unexpected runtime, identity, profile or agent; refusing continuation');
  }
  show(id) { return this.call(['show', id, '--json']); }
  output(id, after = '') {
    // Twenty 64 KiB events can grow sixfold under JSON escaping.
    return this.call(['output', id, ...(after ? ['--after', after] : []), '--limit', '20', '--json'], 16 * 1024 * 1024);
  }
  async message(id, prompt, externalId) {
    try { return await this.call(['message', id, prompt, '--external-id', externalId, '--json']); }
    catch (error) {
      // The CLI can emit a durable receipt then exit nonzero for offline/uncertain delivery.
      if (error.stdout) {
        const receipt = JSON.parse(error.stdout);
        if (['pending', 'uncertain', 'claimed'].includes(receipt.status)) return receipt;
      }
      throw error;
    }
  }
}
