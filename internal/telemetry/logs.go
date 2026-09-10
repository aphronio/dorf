package telemetry

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/version"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
)

type executionKey struct{}

func WithExecution(ctx context.Context, run core.AgentRun) context.Context {
	return context.WithValue(ctx, executionKey{}, run)
}

func Execution(ctx context.Context) (core.AgentRun, bool) {
	run, ok := ctx.Value(executionKey{}).(core.AgentRun)
	return run, ok && run.JobID != "" && run.MessageID != "" && run.ID != ""
}

type Event struct {
	Name       string
	At         time.Time
	Attributes map[string]any
	Failed     bool
}

type Publisher struct {
	provider *sdklog.LoggerProvider
	logger   log.Logger
}

// A logs endpoint opts this process into execution diagnostics. The official
// exporter owns authentication, batching and bounded retries through OTEL_*.
func FromEnv(ctx context.Context) (*Publisher, error) {
	if strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT")) == "" {
		return nil, nil
	}
	resources, err := resource.New(ctx, resource.WithFromEnv(), resource.WithAttributes(
		attribute.String("service.name", "dorf"),
		attribute.String("service.version", version.Version),
	))
	if err != nil {
		return nil, err
	}
	exporter, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, err
	}
	provider := sdklog.NewLoggerProvider(
		sdklog.WithResource(resources),
		sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
	)
	return &Publisher{provider: provider, logger: provider.Logger("dorf.execution")}, nil
}

func (p *Publisher) Emit(event Event) {
	var record log.Record
	record.SetEventName(event.Name)
	record.SetBody(log.StringValue(event.Name))
	record.SetTimestamp(event.At)
	record.SetObservedTimestamp(time.Now())
	record.SetSeverity(log.SeverityInfo)
	if event.Failed {
		record.SetSeverity(log.SeverityError)
	}
	for key, value := range event.Attributes {
		record.AddAttributes(log.KeyValue{Key: key, Value: logValue(value)})
	}
	p.logger.Emit(context.Background(), record)
}

func (p *Publisher) Shutdown(ctx context.Context) error { return p.provider.Shutdown(ctx) }

func logValue(value any) log.Value {
	switch value := value.(type) {
	case string:
		return log.StringValue(value)
	case bool:
		return log.BoolValue(value)
	case float64:
		return log.Float64Value(value)
	case int64:
		return log.Int64Value(value)
	case map[string]any:
		fields := make([]log.KeyValue, 0, len(value))
		for key, field := range value {
			fields = append(fields, log.KeyValue{Key: key, Value: logValue(field)})
		}
		return log.MapValue(fields...)
	case []any:
		values := make([]log.Value, len(value))
		for index, field := range value {
			values[index] = logValue(field)
		}
		return log.SliceValue(values...)
	default:
		return log.Value{}
	}
}
