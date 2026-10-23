package transcript

// LatestRunRecords selects the last independently started execution in an
// accumulating audit file. NewWriter starts a root (empty ParentUUID), whereas
// ResumeWriter keeps linking to its existing run. Control/exclusion records do
// not start executions. No audit records are modified or removed.
func LatestRunRecords(recs []Record) []Record {
	start := 0
	for i, r := range recs {
		if r.Type == "message" && r.Message != nil && r.UUID != "" && r.ParentUUID == "" {
			start = i
		}
	}
	return recs[start:]
}
