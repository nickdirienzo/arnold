import { createHash } from 'node:crypto';

// These are transport chunks, not a second copy of the agent conversation.
export function slackChunks(text, size = 3500) {
  const characters = Array.from(text);
  const chunks = [];
  for (let i = 0; i < characters.length; i += size) chunks.push(characters.slice(i, i + size).join(''));
  return chunks;
}

export async function relayOutput({ task, channel, threadTs, paddock, slack, store, recipient, onTurnFinished }) {
  let after = store.outputCursor(task);
  let partial = null;
  const seenCursors = new Set([after]);
  const post = async (event, text) => {
    const chunks = slackChunks(text);
    for (let i = 0; i < chunks.length; i++) {
      if (store.outputDelivered(task, event.id, i)) continue;
      if (slack.streamEvent) {
        await slack.streamEvent({ task, event, text: chunks[i], chunk: i, channel, threadTs, recipient, store });
        continue;
      }
      const id = createHash('sha256').update(JSON.stringify(['output', task, event.id, i])).digest('hex');
      await slack.post(channel, threadTs, chunks[i], id);
      store.recordOutput(task, event.id, i);
    }
  };
  do {
    const page = await paddock.output(task, after);
    if (page.task_id !== task || !Array.isArray(page.events) || typeof page.has_more !== 'boolean' ||
        (page.next_cursor !== undefined && typeof page.next_cursor !== 'string')) throw new Error('Invalid Paddock output page');
    const next = page.next_cursor ?? '';
    if (page.events.length && (!next || seenCursors.has(next))) throw new Error('Paddock output cursor did not advance');
    if (!page.events.length && (page.has_more || next !== after)) throw new Error('Invalid empty Paddock output page');
    for (const event of page.events) {
      if (!event.id || !event.turn_id) throw new Error('Paddock output event has no stable ID or turn');
      if (event.type === 'message') {
        if (!['commentary', 'partial_answer', 'final_answer'].includes(event.phase) || typeof event.text !== 'string' || !event.text)
          throw new Error('Invalid public Paddock message');
        const part = event.part ?? 1;
        const parts = event.parts ?? 1;
        if (!Number.isSafeInteger(part) || !Number.isSafeInteger(parts) || part < 1 || parts < part)
          throw new Error('Invalid Paddock message parts');
        if (!partial) {
          if (part !== 1) throw new Error('Missing first Paddock message part');
          partial = { event, parts, next: 1, text: [] };
        }
        if (part !== partial.next || parts !== partial.parts || event.turn_id !== partial.event.turn_id || event.phase !== partial.event.phase)
          throw new Error('Paddock message parts are not contiguous');
        partial.text.push(event.text);
        partial.next++;
        if (part === parts) {
          await post(partial.event, partial.text.join(''));
          partial = null;
        }
      } else if (event.type === 'turn_finished') {
        if (partial || !['completed', 'failed', 'interrupted'].includes(event.state)) throw new Error('Invalid Paddock turn completion');
        if (slack.streamEvent) {
          if (!store.outputDelivered(task, event.id, 0))
            await slack.streamEvent({ task, event, chunk: 0, channel, threadTs, recipient, store });
        } else {
          if (event.state === 'failed') await post(event, "I couldn't finish that request. Please try again.");
          if (event.state === 'interrupted') await post(event, 'That request was interrupted. Please try again.');
        }
        await onTurnFinished?.(event);
      } else throw new Error('Unknown Paddock output event type');
    }
    // A partial message is fetched again after a crash. Earlier Slack chunks have
    // durable receipts, so the delivered cursor never skips undelivered text.
    if (!partial) store.saveOutputCursor(task, next);
    after = next;
    seenCursors.add(next);
    if (!page.has_more) {
      if (partial) throw new Error('Paddock output ended before the message was complete');
      return;
    }
  } while (true);
}
