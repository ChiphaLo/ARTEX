package db

// ConversationTokenTotal covers the whole conversation, independent of the
// transcript page loaded by a client. Each usage row is cumulative within one
// run; results seal that run. Never sum usage rows or count a sealed run twice.
type ConversationTokenTotal struct {
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
	Turns            int `json:"turns"`
}

func (d *DB) ConversationTokens(id int64) (ConversationTokenTotal, error) {
	var total ConversationTokenTotal
	err := d.QueryRow(`
WITH accounting AS (
    SELECT id, kind, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
    FROM conversation_activities
    WHERE conversation_id=$1 AND kind IN ('result', 'usage')
), sealed AS (
    SELECT COALESCE(sum(input_tokens) FILTER (WHERE kind='result'),0) AS input,
           COALESCE(sum(output_tokens) FILTER (WHERE kind='result'),0) AS output,
           COALESCE(sum(cache_read_tokens) FILTER (WHERE kind='result'),0) AS cache_read,
           COALESCE(sum(cache_write_tokens) FILTER (WHERE kind='result'),0) AS cache_write,
           COALESCE(max(id) FILTER (WHERE kind='result'),0) AS last_result,
           count(*) FILTER (WHERE kind='usage') AS turns
    FROM accounting
)
SELECT sealed.input + COALESCE(live.input_tokens,0),
       sealed.output + COALESCE(live.output_tokens,0),
       sealed.cache_read + COALESCE(live.cache_read_tokens,0),
       sealed.cache_write + COALESCE(live.cache_write_tokens,0), sealed.turns
FROM sealed
LEFT JOIN LATERAL (
    SELECT input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
    FROM accounting WHERE kind='usage' AND id>sealed.last_result
    ORDER BY id DESC LIMIT 1
) live ON true`, id).Scan(&total.InputTokens, &total.OutputTokens,
		&total.CacheReadTokens, &total.CacheWriteTokens, &total.Turns)
	return total, err
}
