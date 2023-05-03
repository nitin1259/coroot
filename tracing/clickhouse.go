package tracing

import (
	"context"
	"fmt"
	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/coroot/coroot/model"
	"github.com/coroot/coroot/timeseries"
	"github.com/coroot/coroot/utils"
	"strings"
	"time"
)

type ClickhouseClient struct {
	conn clickhouse.Conn
}

func NewClickhouseClient(addr string, auth *utils.BasicAuth) (*ClickhouseClient, error) {
	var user, password string
	if auth != nil {
		user, password = auth.User, auth.Password
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{addr},
		Auth: clickhouse.Auth{
			Database: "default",
			Username: user,
			Password: password,
		},
		Compression: &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return &ClickhouseClient{conn: conn}, nil
}

func (c *ClickhouseClient) Ping(ctx context.Context) error {
	return c.conn.Ping(ctx)
}

func (c *ClickhouseClient) GetServiceNames(ctx context.Context, from, to timeseries.Time) ([]string, error) {
	q := "SELECT ServiceName FROM otel_traces WHERE Timestamp BETWEEN @from AND @to GROUP BY ServiceName"
	rows, err := c.conn.Query(ctx, q,
		clickhouse.DateNamed("from", from.ToStandard(), clickhouse.NanoSeconds),
		clickhouse.DateNamed("to", to.ToStandard(), clickhouse.NanoSeconds),
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()
	var res []string
	for rows.Next() {
		var app string
		if err = rows.Scan(&app); err != nil {
			return nil, err
		}
		res = append(res, app)
	}
	return res, nil
}

func (c *ClickhouseClient) GetSpansByServiceName(ctx context.Context, name string, ignoredPeerAddrs []string, tsFrom, tsTo timeseries.Time, durFrom, durTo time.Duration, errors bool, limit int) ([]*Span, error) {
	return c.getSpans(ctx, tsFrom, tsTo, durFrom, durTo, errors, "Timestamp DESC", limit,
		`
			ServiceName = @name AND 
			SpanKind = 'SPAN_KIND_SERVER' AND 
			SpanAttributes['net.sock.peer.addr'] NOT IN (@addrs)
		`,
		clickhouse.Named("name", name),
		clickhouse.Named("addrs", ignoredPeerAddrs),
	)
}

func (c *ClickhouseClient) GetInboundSpans(ctx context.Context, listens []model.Listen, ignoredContainerIds []string, tsFrom, tsTo timeseries.Time, durFrom, durTo time.Duration, errors bool, limit int) ([]*Span, error) {
	if len(listens) == 0 {
		return nil, nil
	}
	ips := utils.NewStringSet()
	for _, l := range listens {
		if l.Port == "0" {
			ips.Add(l.IP)
		}
	}
	var addrs []clickhouse.GroupSet
	for _, l := range listens {
		addrs = append(addrs, clickhouse.GroupSet{Value: []any{l.IP, l.Port}})
	}
	return c.getSpans(ctx, tsFrom, tsTo, durFrom, durTo, errors, "Timestamp DESC", limit,
		`
			ServiceName = 'coroot-node-agent' AND 
			(SpanAttributes['net.peer.name'] IN (@ips) OR (SpanAttributes['net.peer.name'], SpanAttributes['net.peer.port']) IN (@addrs)) 
			AND SpanAttributes['container.id'] NOT IN (@containerIds)
		`,
		clickhouse.Named("ips", ips.Items()),
		clickhouse.Named("addrs", addrs),
		clickhouse.Named("containerIds", ignoredContainerIds),
	)
}

func (c *ClickhouseClient) GetParentSpans(ctx context.Context, spans []*Span, tsFrom, tsTo timeseries.Time) ([]*Span, error) {
	var ids []clickhouse.GroupSet
	for _, s := range spans {
		if s.ParentSpanId != "" {
			ids = append(ids, clickhouse.GroupSet{Value: []any{s.TraceId, s.ParentSpanId}})
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return c.getSpans(ctx, tsFrom, tsTo, 0, 0, false, "", 0,
		"(TraceId, SpanId) IN (@ids)",
		clickhouse.Named("ids", ids),
	)
}

func (c *ClickhouseClient) GetSpansByTraceId(ctx context.Context, traceId string) ([]*Span, error) {
	return c.getSpans(ctx, 0, 0, 0, 0, false, "Timestamp", 0,
		"TraceId = @traceId AND Timestamp BETWEEN (SELECT min(Start) FROM otel_traces_trace_id_ts WHERE TraceId = @traceId) AND (SELECT max(End) + 1 FROM otel_traces_trace_id_ts WHERE TraceId = @traceId)",
		clickhouse.Named("traceId", traceId),
	)
}

func (c *ClickhouseClient) getSpans(ctx context.Context, tsFrom, tsTo timeseries.Time, durFrom, durTo time.Duration, errors bool, orderBy string, limit int, filter string, filterArgs ...any) ([]*Span, error) {
	var filters []string
	var args []any

	if !tsFrom.IsZero() && !tsTo.IsZero() {
		filters = append(filters, "Timestamp BETWEEN @tsFrom AND @tsTo")
		args = append(args,
			clickhouse.DateNamed("tsFrom", tsFrom.ToStandard(), clickhouse.NanoSeconds),
			clickhouse.DateNamed("tsTo", tsTo.ToStandard(), clickhouse.NanoSeconds),
		)
	}

	if filter != "" {
		filters = append(filters, filter)
		args = append(args, filterArgs...)
	}

	switch {
	case durFrom > 0 && durTo > 0 && errors:
		filters = append(filters, "(Duration BETWEEN @durFrom AND @durTo OR StatusCode = 'STATUS_CODE_ERROR')")
	case durFrom == 0 && durTo > 0 && errors:
		filters = append(filters, "(Duration <= @durTo OR StatusCode = 'STATUS_CODE_ERROR')")
	case durFrom > 0 && durTo == 0 && errors:
		filters = append(filters, "(Duration >= @durFrom OR StatusCode = 'STATUS_CODE_ERROR')")
	case durFrom == 0 && durTo == 0 && errors:
		filters = append(filters, "StatusCode = 'STATUS_CODE_ERROR'")
	case durFrom > 0 && durTo > 0 && !errors:
		filters = append(filters, "Duration BETWEEN @durFrom AND @durTo")
	case durFrom == 0 && durTo > 0 && !errors:
		filters = append(filters, "Duration <= @durTo")
	case durFrom > 0 && durTo == 0 && !errors:
		filters = append(filters, "Duration >= @durFrom")
	}
	if durFrom > 0 {
		args = append(args, clickhouse.Named("durFrom", durFrom.Nanoseconds()))
	}
	if durTo > 0 {
		args = append(args, clickhouse.Named("durTo", durTo.Nanoseconds()))
	}

	q := "SELECT Timestamp, TraceId, SpanId, ParentSpanId, SpanName, ServiceName, Duration, StatusCode, StatusMessage, SpanAttributes, Events.Timestamp, Events.Name, Events.Attributes"
	q += " FROM otel_traces"
	q += " WHERE " + strings.Join(filters, " AND ")
	if orderBy != "" {
		q += " ORDER BY " + orderBy
	}
	if limit > 0 {
		q += " LIMIT " + fmt.Sprint(limit)
	}

	rows, err := c.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rows.Close()
	}()
	var res []*Span
	for rows.Next() {
		var s Span
		var eventsTimestamp []time.Time
		var eventsName []string
		var eventsAttributes []map[string]string
		if err = rows.Scan(&s.Timestamp, &s.TraceId, &s.SpanId, &s.ParentSpanId, &s.Name, &s.ServiceName, &s.Duration,
			&s.StatusCode, &s.StatusMessage, &s.Attributes, &eventsTimestamp, &eventsName, &eventsAttributes,
		); err != nil {
			return nil, err
		}
		l := len(eventsTimestamp)
		if l > 0 && l == len(eventsName) && l == len(eventsAttributes) {
			s.Events = make([]Event, l, l)
			for i := range eventsTimestamp {
				s.Events[i].Timestamp = eventsTimestamp[i]
				s.Events[i].Name = eventsName[i]
				s.Events[i].Attributes = eventsAttributes[i]
			}
		}
		res = append(res, &s)
	}
	return res, nil
}