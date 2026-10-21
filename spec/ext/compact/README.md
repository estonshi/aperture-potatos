# ext/compact

The default compaction strategy. Every text the model sees is a file here, sent byte for byte.

## Summarizing

- A context with fewer than 4 lines after the epoch head, or, for a full compaction that is not an overflow, one no larger than the summary target, has nothing to compact.
- The instructions are the template (`summary.md`, or the caller's own template), then, for a caller's template when the epoch starts from an earlier summary, the sentence in `carry_forward.md`, then the per-call instructions. `{{target}}` is the summary target in tokens. Every template is rendered by the language of `runtime.md` §7; a caller's template that does not parse fails the install.
- A full compaction summarizes with the whole context as the cached prefix. It keeps the running turn verbatim from its first line, unless the caller drops it or the model's thinking signatures bind the prefix; then it keeps nothing.
- After an overflow the context no longer fits one request. The strategy summarizes the oldest half and the rest as given lines, splitting at the user-side line nearest the middle that starts no tool result, halving a part again when it overflows too, at most 3 levels deep. It then merges the summaries with the template followed by `merge.md`. The carry-forward sentence goes only on the part that starts with the epoch head. Before that, while the running turn's lines are estimated at more than half the threshold, its largest tool result not yet cleared is cleared, as pruning clears one; what is summarized and what is kept of the turn are both the lines after this clearing, since a turn that alone fills the window could be neither.
- Up to a turn: the lines before it are summarized as given lines, and the turn and the rest are kept.
- From a turn: `summary_from.md`, with `{{start}}` the first 200 code points of the turn's first line, then the per-call instructions, with the whole context as the cached prefix.
- An empty summary fails the compaction.

## Clearing old tool results

With pruning on, an automatic or overflow compaction of a full context, on a model whose signatures do not bind the prefix, first tries clearing tool results. It considers the results of lines before the running turn, leaves the most recent `keep_recent` of them (default 5; a negative value keeps none), and clears those whose content is at least `min_tokens` (default 1000) by the runtime's heuristic. When at least one is cleared and the estimate minus what clearing removed is at most half the threshold, the result keeps every line, with those results cleared, keeps the earlier summary, and has strategy `prune`. Otherwise the strategy summarizes the original lines.
