package tlsedge

import (
	"context"
	"log/slog"

	"go.uber.org/zap/zapcore"
)

// slogZapCore is a tiny zapcore.Core that forwards CertMagic's zap log records
// into the application's slog.Logger, so on-demand TLS issuance/renewal/errors
// land in the SAME structured stream as the rest of kotoji (no second log sink).
//
// It is intentionally minimal: zap fields are accumulated as slog attributes via
// a memory encoder, levels are mapped zap->slog, and everything at >= Info is
// emitted. CertMagic uses this only for its own diagnostics; the bridge keeps the
// engine dependency-light (no zap production config / file rotation).
type slogZapCore struct {
	logger *slog.Logger
	fields []zapcore.Field
}

// newSlogZapCore builds the bridge core around logger (must be non-nil).
func newSlogZapCore(logger *slog.Logger) zapcore.Core {
	return &slogZapCore{logger: logger}
}

// Enabled reports whether a level is logged. We forward Info and above; Debug is
// dropped to keep CertMagic's verbose handshake tracing out of normal logs.
func (c *slogZapCore) Enabled(l zapcore.Level) bool { return l >= zapcore.InfoLevel }

// With accumulates structured context, returning a derived core that carries the
// extra fields on every subsequent Write (zap's With contract).
func (c *slogZapCore) With(fields []zapcore.Field) zapcore.Core {
	merged := make([]zapcore.Field, 0, len(c.fields)+len(fields))
	merged = append(merged, c.fields...)
	merged = append(merged, fields...)
	return &slogZapCore{logger: c.logger, fields: merged}
}

// Check adds this core to the checked entry iff the entry's level is enabled.
func (c *slogZapCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

// Write emits one record to slog, mapping the zap level and flattening the
// accumulated + call-site fields to slog attributes.
func (c *slogZapCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	attrs := zapFieldsToAttrs(append(c.fields, fields...))
	c.logger.LogAttrs(context.Background(), zapLevelToSlog(ent.Level), ent.Message, attrs...)
	return nil
}

// Sync is a no-op: slog handlers flush per-record.
func (c *slogZapCore) Sync() error { return nil }

// zapLevelToSlog maps a zap level onto the closest slog level.
func zapLevelToSlog(l zapcore.Level) slog.Level {
	switch {
	case l >= zapcore.ErrorLevel:
		return slog.LevelError
	case l == zapcore.WarnLevel:
		return slog.LevelWarn
	case l == zapcore.InfoLevel:
		return slog.LevelInfo
	default:
		return slog.LevelDebug
	}
}

// zapFieldsToAttrs flattens zap fields into slog attributes. It uses zap's own
// MapObjectEncoder to materialize each field's value (covering strings, ints,
// errors, durations, etc.) without re-implementing every zap field type.
func zapFieldsToAttrs(fields []zapcore.Field) []slog.Attr {
	if len(fields) == 0 {
		return nil
	}
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range fields {
		f.AddTo(enc)
	}
	attrs := make([]slog.Attr, 0, len(enc.Fields))
	for k, v := range enc.Fields {
		attrs = append(attrs, slog.Any(k, v))
	}
	return attrs
}
