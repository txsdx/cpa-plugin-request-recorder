package recorder

func requestTotals(r *Record) ConversationTotals {
	totals := ConversationTotals{}
	if r.DurationMS > 0 {
		totals.DurationMS = r.DurationMS
		totals.DurationRequests = 1
	} else if !r.StartedAt.IsZero() && !r.CompletedAt.IsZero() && !r.CompletedAt.Before(r.StartedAt) {
		totals.DurationMS = ms(r.CompletedAt.Sub(r.StartedAt))
		totals.DurationRequests = 1
	}
	if r.Tokens != nil {
		totals.TotalTokens = r.Tokens.TotalTokens
		totals.TokenRequests = 1
	}
	return totals
}

func conversationTotals(r *Record) ConversationTotals {
	if r.ConversationTotals != nil {
		return *r.ConversationTotals
	}
	// A record without cumulative counters only knows its latest request metrics.
	return requestTotals(r)
}

func updateConversationTotals(r, previous *Record) {
	totals := requestTotals(r)
	if previous != nil {
		before := conversationTotals(previous)
		if previous.RequestID == r.RequestID {
			// Metadata and repeated/late usage updates replace this request's
			// contribution instead of counting the same model execution twice.
			latest := requestTotals(previous)
			before.DurationMS -= latest.DurationMS
			before.TotalTokens -= latest.TotalTokens
			before.DurationRequests -= latest.DurationRequests
			before.TokenRequests -= latest.TokenRequests
		}
		totals.DurationMS += before.DurationMS
		totals.TotalTokens += before.TotalTokens
		totals.DurationRequests += before.DurationRequests
		totals.TokenRequests += before.TokenRequests
	}
	r.ConversationTotals = &totals
}
