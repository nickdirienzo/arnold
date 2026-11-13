import { createHash } from 'node:crypto';

export class Slack {
  constructor(client) { this.client = client; }
  async context(channel, ts, exclude) {
    const messages = [];
    let cursor;
    let more = false;
    // Bound both API work and prompt size. Missing access is an explicit failure.
    for (let page = 0; page < 4; page++) {
      const r = await this.client.conversations.replies({ channel, ts, limit: 100, cursor });
      for (const m of r.messages ?? []) {
        if (m.ts !== exclude && Number(m.ts) < Number(exclude))
          messages.push(`${m.ts} [${m.user ?? m.bot_id ?? 'unknown'}]: ${m.text ?? ''}`);
      }
      cursor = r.response_metadata?.next_cursor;
      more = Boolean(cursor);
      if (!cursor) break;
    }
    const text = messages.join('\n');
    return (more || text.length > 24000 ? '[Context truncated]\n' : '') + text.slice(-24000);
  }
  post(channel, ts, text, id) {
    // Disable mention parsing and unfurls for deterministic status messages.
    return this.client.chat.postMessage({ channel, thread_ts: ts, text,
      client_msg_id: `${id.slice(0, 8)}-${id.slice(8, 12)}-4${id.slice(13, 16)}-8${id.slice(17, 20)}-${id.slice(20, 32)}`,
      unfurl_links: false, unfurl_media: false, parse: 'none' });
  }
  async reaction(channel, timestamp, add) {
    try {
      await this.client.reactions[add ? 'add' : 'remove']({ channel, timestamp, name: 'eyes' });
    } catch (error) {
      const code = error.data?.error;
      if ((add && code === 'already_reacted') || (!add && code === 'no_reaction')) return;
      throw error;
    }
  }
  work(task, status = 'in_progress', details) {
    return { type: 'task_update', id: 'paddock', title: 'Engineering task', status,
      details: `Paddock task ${task}${details ? `\n${details}` : ''}` };
  }
  async stopStream(channel, ts, chunks, continuing = false) {
    try {
      await this.client.chat.stopStream({ channel, ts, chunks, session_status: continuing ? 'processing' : 'active' });
    } catch (error) {
      // Slack automatically finalizes streams after ten minutes. An expired
      // stream is already closed; its original output remains in Slack.
      if (error.data?.error !== 'message_not_in_streaming_state') throw error;
    }
  }
  async streamEvent({ task, event, text, chunk, channel, threadTs, recipient, store }) {
    const turn = event.turn_id;
    let metadata = store.stream(task, turn);
    // Channel streams require a human recipient. Automatic conversations can
    // configure one; without one, relay answers normally and keep logs local.
    if (!recipient?.userId) {
      const answer = event.type === 'message' && event.phase !== 'commentary';
      const failure = event.type === 'turn_finished' && event.state !== 'completed';
      if (answer || failure) {
        const id = createHash('sha256').update(JSON.stringify(['output', task, event.id, chunk])).digest('hex');
        await this.post(channel, threadTs, text ?? `The request ${event.state}. Please try again.`, id);
      }
      store.recordOutput(task, event.id, chunk);
      return;
    }
    if (metadata?.finished) throw new Error('Cannot append output to a finished Slack turn');
    if (event.type === 'turn_finished' && !metadata?.ts && event.state === 'completed') {
      store.commitStream(task, turn, { finished: true }, event.id, chunk);
      return;
    }
    const markdown = event.type === 'message' && event.phase !== 'commentary';
    const rotate = metadata?.ts && (Date.now() - metadata.started >= 9 * 60_000 ||
      metadata.characters + (markdown ? text.length : 0) > 10_000 || metadata.tasks >= 48);
    if (rotate) {
      await this.stopStream(channel, metadata.ts, [this.work(task, 'complete', 'Continuing in the next message')], true);
      metadata = { finished: false };
      store.saveStream(task, turn, metadata);
    }
    if (!metadata?.ts) {
      const started = await this.client.chat.startStream({ channel, thread_ts: threadTs,
        recipient_team_id: recipient.teamId, recipient_user_id: recipient.userId,
        task_display_mode: 'plan', chunks: [{ type: 'plan_update', title: 'Working' },
          this.work(task, 'in_progress', `Request received; execution isolated in OpenShell.\n${store.status(task) ?? ''}`)] });
      if (!started.ts) throw new Error('Slack did not return a stream timestamp');
      metadata = { ts: started.ts, started: Date.now(), characters: 0, tasks: 1, finished: false };
      store.saveStream(task, turn, metadata);
    }
    if (event.type === 'turn_finished') {
      const success = event.state === 'completed';
      const chunks = [this.work(task, success ? 'complete' : 'error', `Turn ${turn}: ${event.state}`)];
      if (!success) chunks.push({ type: 'markdown_text', text: `The request ${event.state}. Please try again.` });
      await this.stopStream(channel, metadata.ts, chunks);
      metadata.finished = true;
    } else {
      const update = markdown ? { type: 'markdown_text', text } : {
        type: 'task_update', id: createHash('sha256').update(`${event.id}:${chunk}`).digest('hex').slice(0, 32),
        title: Array.from(text).slice(0, 160).join(''), details: text, status: 'complete',
      };
      try {
        await this.client.chat.appendStream({ channel, ts: metadata.ts, chunks: [update] });
      } catch (error) {
        if (error.data?.error !== 'message_not_in_streaming_state') throw error;
        store.saveStream(task, turn, { finished: false });
        return this.streamEvent({ task, event, text, chunk, channel, threadTs, recipient, store });
      }
      if (markdown) metadata.characters += text.length;
      else metadata.tasks++;
    }
    store.commitStream(task, turn, metadata, event.id, chunk);
  }
  async streamStatus({ task, channel, status, store }) {
    for (const stream of store.activeStreams(task)) {
      try {
        await this.client.chat.appendStream({ channel, ts: stream.ts, chunks: [this.work(task, 'in_progress', status)] });
      } catch (error) {
        if (error.data?.error !== 'message_not_in_streaming_state') throw error;
        store.saveStream(task, stream.turn, { finished: false });
      }
    }
  }
}
