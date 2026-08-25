package transport

// WithFallback fills absent negotiated values from fallback. A negotiated
// value of zero means that the peer did not provide that optional limit.
func (l Limits) WithFallback(fallback Limits) Limits {
	if l.MaxFrameBytes <= 0 {
		l.MaxFrameBytes = fallback.MaxFrameBytes
	}
	if l.MaxRecordBytes <= 0 {
		l.MaxRecordBytes = fallback.MaxRecordBytes
	}
	if l.MaxWriteBatchBytes <= 0 {
		l.MaxWriteBatchBytes = fallback.MaxWriteBatchBytes
	}
	if l.MaxUDPBytes <= 0 {
		l.MaxUDPBytes = fallback.MaxUDPBytes
	}
	if l.MaxStreams <= 0 {
		l.MaxStreams = fallback.MaxStreams
	}
	return l
}

// EffectivePadding caps configured padding to the negotiated relay record
// payload budget. A missing record limit preserves the configured value so
// callers can apply their local fallback first.
func (l Limits) EffectivePadding(configured int64) int64 {
	if configured <= 0 {
		return 0
	}
	if max := l.MaxRecordBytes - 12; l.MaxRecordBytes > 0 && max >= 0 && configured > max {
		return max
	}
	return configured
}
