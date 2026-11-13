`node-state.sqlite` was produced by the original JavaScript Store using only
synthetic workspace/user IDs, a synthetic task ID and placeholder conversation
text. It contains thread/identity bindings, a completed receipt, a frozen pending
follow-up, an output cursor/receipt, reaction state and active stream metadata.

The migration test copies it to a temporary directory before opening it with the
Go implementation. No production state or credentials are included.
