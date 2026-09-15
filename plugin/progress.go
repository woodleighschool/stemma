package plugin

import (
	"context"
	"io"
	"time"
)

// ProgressReader reports bytes consumed without buffering or changing read errors.
// A non-positive total means the size is unknown. Counts describe transfer, not
// subsequent verification or acceptance by a remote service.
func ProgressReader(ctx context.Context, reader io.Reader, total int64) io.Reader {
	p := &progressReader{ctx: ctx, reader: reader, total: total}
	p.report(false)
	return p
}

type progressReader struct {
	ctx            context.Context
	reader         io.Reader
	current, total int64
	last           time.Time
}

func (p *progressReader) Read(data []byte) (int, error) {
	n, err := p.reader.Read(data)
	p.current += int64(n)
	if err != nil || time.Since(p.last) >= 250*time.Millisecond {
		p.report(err != nil)
	}
	return n, err
}

func (p *progressReader) report(final bool) {
	args := []any{"progress", true, "current", p.current, "unit", "bytes", "progress_final", final}
	if p.total > 0 {
		args = append(args, "total", p.total)
	}
	Logger(p.ctx).InfoContext(p.ctx, "Transfer progress", args...)
	p.last = time.Now()
}
