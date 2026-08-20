// Package metrics holds the Prometheus collectors for the notifier. It is a
// singleton registered against the default registry so any package can
// record without threading a handle through.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// NotificationsDelivered counts successfully delivered notifications by
	// channel and ingest kind.
	NotificationsDelivered = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_notifier_notifications_delivered_total",
		Help: "Notifications delivered to a Matrix room.",
	}, []string{"channel", "kind"})

	// NotificationsFailed counts notifications that could not be delivered.
	NotificationsFailed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_notifier_notifications_failed_total",
		Help: "Notifications that failed to deliver.",
	}, []string{"channel", "kind"})

	// IngestRejected counts requests refused before delivery (auth, rate
	// limit, bad body).
	IngestRejected = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_notifier_ingest_rejected_total",
		Help: "Ingest requests rejected before delivery.",
	}, []string{"kind", "reason"})

	// SendRetries counts individual send retry attempts.
	SendRetries = promauto.NewCounter(prometheus.CounterOpts{
		Name: "matrix_notifier_send_retries_total",
		Help: "Individual Matrix send retry attempts.",
	})

	// SendDuration measures end-to-end send latency (all retries included).
	SendDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "matrix_notifier_send_duration_seconds",
		Help:    "Time to deliver a message to Matrix, including retries.",
		Buckets: prometheus.DefBuckets,
	})

	// ChartRenders counts chart attachment attempts by outcome.
	ChartRenders = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "matrix_notifier_chart_renders_total",
		Help: "Prometheus chart render attempts.",
	}, []string{"outcome"})

	// ChartDuration measures chart query+render time.
	ChartDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "matrix_notifier_chart_duration_seconds",
		Help:    "Time to query Prometheus and render a chart.",
		Buckets: prometheus.DefBuckets,
	})

	// SyncAge is the seconds since the last successful Matrix sync. A rising
	// value means the bot is disconnected — alert on it.
	SyncAge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "matrix_notifier_sync_age_seconds",
		Help: "Seconds since the last successful Matrix sync.",
	})

	// OutboxPending is the number of notifications queued for delivery. A
	// rising value means Matrix sends are failing — alert on it.
	OutboxPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "matrix_notifier_outbox_pending",
		Help: "Notifications queued and awaiting delivery.",
	})

	// KeyShareEmpty counts sends refused because no recipient device could
	// be resolved for the room: the megolm session would have been shared
	// with nobody and the message would arrive undecryptable. Alert on any
	// increase — this is the signal the silent key-share failure lacked.
	KeyShareEmpty = promauto.NewCounter(prometheus.CounterOpts{
		Name: "matrix_notifier_key_share_empty_total",
		Help: "Sends refused because no recipient device was known for the room.",
	})

	// SessionsDiscarded counts stored megolm sessions thrown away because
	// they had reached no device: every message encrypted with them was
	// undecryptable. A non-zero value means a poisoned session was found
	// and recovered from.
	SessionsDiscarded = promauto.NewCounter(prometheus.CounterOpts{
		Name: "matrix_notifier_megolm_sessions_discarded_total",
		Help: "Stored megolm sessions discarded because they reached no device.",
	})

	// Verified reports whether the bot's device is cross-signed (1) or not.
	Verified = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "matrix_notifier_device_verified",
		Help: "1 if the bot device is cross-signed and verified, else 0.",
	})
)
