package controller

func batchIDs(values []string, start, size int) ([]string, []any, int) {
	end := start + size
	if end > len(values) {
		end = len(values)
	}
	ids := append([]string(nil), values[start:end]...)
	args := make([]any, len(ids))
	for index, id := range ids {
		args[index] = id
	}
	return ids, args, end
}
