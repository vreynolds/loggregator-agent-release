package binding

import (
	"code.cloudfoundry.org/go-loggregator/metrics"
	"context"
	"log"
	"math/rand"
	"sync"
	"time"

	"code.cloudfoundry.org/loggregator-agent/pkg/egress"
	"code.cloudfoundry.org/loggregator-agent/pkg/egress/syslog"
)

type Fetcher interface {
	FetchBindings() ([]syslog.Binding, error)
	DrainLimit() int
}

type Metrics interface {
	NewGauge(name string, o ...metrics.MetricOption) metrics.Gauge
	NewCounter(name string, o ...metrics.MetricOption) metrics.Counter
}

type Connector interface {
	Connect(context.Context, syslog.Binding) (egress.Writer, error)
}

type Manager struct {
	bf                 Fetcher
	connector          Connector
	aggregateDrains    []drainHolder
	aggregateDrainURLs []string

	pollingInterval time.Duration
	idleTimeout     time.Duration

	drainCountMetric          metrics.Gauge
	aggregateDrainCountMetric metrics.Gauge
	activeDrainCountMetric    metrics.Gauge
	activeDrainCount          int64

	sourceDrainMap    map[string]map[syslog.Binding]drainHolder
	sourceAccessTimes map[string]time.Time

	log *log.Logger
	mu  sync.Mutex
}

func NewManager(
	bf Fetcher,
	c Connector,
	aggregateDrainURLs []string,
	m Metrics,
	pollingInterval time.Duration,
	idleTimeout time.Duration,
	log *log.Logger,
) *Manager {
	tagOpt := metrics.WithMetricTags(map[string]string{"unit": "count"})
	drainCount := m.NewGauge(
		"drains",
		metrics.WithHelpText("Current number of syslog drain bindings."),
		tagOpt,
	)
	aggregateDrainCount := m.NewGauge(
		"aggregate_drains",
		metrics.WithHelpText("Current number of aggregate drains."),
		tagOpt,
	)
	activeDrains := m.NewGauge(
		"active_drains",
		metrics.WithHelpText("Current number of active syslog drains including app and aggregate drains."),
		tagOpt,
	)

	manager := &Manager{
		bf:                        bf,
		connector:                 c,
		aggregateDrainURLs:        aggregateDrainURLs,
		pollingInterval:           pollingInterval,
		idleTimeout:               idleTimeout,
		drainCountMetric:          drainCount,
		aggregateDrainCountMetric: aggregateDrainCount,
		activeDrainCountMetric:    activeDrains,
		sourceDrainMap:            make(map[string]map[syslog.Binding]drainHolder),
		sourceAccessTimes:         make(map[string]time.Time),
		log:                       log,
	}

	go manager.idleCleanupLoop()

	return manager
}

func (m *Manager) Run() {
	bindings, _ := m.bf.FetchBindings()
	m.drainCountMetric.Set(float64(len(bindings)))
	m.updateDrains(bindings)

	offset := rand.Int63n(m.pollingInterval.Nanoseconds())
	t := time.NewTicker(m.pollingInterval + time.Duration(offset))
	for range t.C {
		bindings, err := m.bf.FetchBindings()
		if err != nil {
			m.log.Printf("failed to fetch bindings: %s", err)
			continue
		}

		m.drainCountMetric.Set(float64(len(bindings)))
		m.updateDrains(bindings)
	}
}

func (m *Manager) GetDrains(sourceID string) []egress.Writer {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sourceAccessTimes[sourceID] = time.Now()
	var drains []egress.Writer
	for binding, drainHolder := range m.sourceDrainMap[sourceID] {
		// Create drain writer if one does not already exist
		if drainHolder.drainWriter == nil {
			writer, err := m.connector.Connect(drainHolder.ctx, binding)
			if err != nil {
				m.log.Printf("failed to create binding: %s", err)
				continue
			}

			drainHolder.drainWriter = writer
			m.sourceDrainMap[sourceID][binding] = drainHolder

			m.updateActiveDrainCount(1)
		}

		drains = append(drains, drainHolder.drainWriter)
	}

	for _, drainHolder := range m.aggregateDrains {
		drains = append(drains, drainHolder.drainWriter)
	}

	return drains
}

func (m *Manager) updateDrains(bindings []syslog.Binding) {
	m.mu.Lock()
	defer m.mu.Unlock()

	newBindings := make(map[syslog.Binding]bool)

	for _, b := range bindings {
		newBindings[b] = true

		_, ok := m.sourceDrainMap[b.AppId][b]
		if ok {
			continue
		}

		_, ok = m.sourceDrainMap[b.AppId]
		if !ok {
			m.sourceDrainMap[b.AppId] = make(map[syslog.Binding]drainHolder)
		}

		m.sourceDrainMap[b.AppId][b] = newDrainHolder()
	}

	// Delete all bindings that are not in updated list of bindings.
	// TODO: this is not optimal, consider lazily storing bindings
	for _, bindingWriterMap := range m.sourceDrainMap {
		for b := range bindingWriterMap {
			if newBindings[b] {
				continue
			}

			m.removeDrain(bindingWriterMap, b)
		}
	}

	for _, drainHolder := range m.aggregateDrains {
		drainHolder.cancel()
	}

	m.updateActiveDrainCount(-int64(len(m.aggregateDrains)))
	m.aggregateDrains = []drainHolder{}
	m.addAggregateDrains(m.aggregateDrainURLs)
}

func (m *Manager) addAggregateDrains(drains []string) {
	for _, drain := range drains {
		aggregateDrainHolder := newDrainHolder()
		writer, err := m.connector.Connect(aggregateDrainHolder.ctx, syslog.Binding{
			AppId: "all",
			Drain: drain,
			Type:  syslog.BINDING_TYPE_AGGREGATE,
		})
		if err != nil {
			m.log.Printf("failed to connect to aggregate drain %s: %s", drain, err)
			continue
		}

		aggregateDrainHolder.drainWriter = writer
		m.aggregateDrains = append(m.aggregateDrains, aggregateDrainHolder)
	}

	m.aggregateDrainCountMetric.Set(float64(len(m.aggregateDrains)))
	m.updateActiveDrainCount(int64(len(m.aggregateDrains)))
}

func (m *Manager) removeDrain(
	bindingWriterMap map[syslog.Binding]drainHolder,
	b syslog.Binding,
) {
	var active bool
	if bindingWriterMap[b].drainWriter != nil {
		active = true
	}

	bindingWriterMap[b].cancel()
	delete(bindingWriterMap, b)
	if len(bindingWriterMap) == 0 {
		// Prevent memory leak
		delete(m.sourceDrainMap, b.AppId)
	}

	if active {
		m.updateActiveDrainCount(-1)
	}
}

func (m *Manager) idleCleanupLoop() {
	t := time.NewTicker(m.idleTimeout)
	for range t.C {
		m.idleCleanup()
	}
}

func (m *Manager) idleCleanup() {
	m.mu.Lock()
	defer m.mu.Unlock()

	currentTime := time.Now()
	for sID, ts := range m.sourceAccessTimes {
		if ts.Before(currentTime.Add(-m.idleTimeout)) {
			for b, dh := range m.sourceDrainMap[sID] {
				dh.cancel()

				m.sourceDrainMap[sID][b] = newDrainHolder()
				m.updateActiveDrainCount(-1)
			}

			delete(m.sourceAccessTimes, sID)
		}
	}
}

func (m *Manager) updateActiveDrainCount(delta int64) {
	m.activeDrainCount += delta
	m.activeDrainCountMetric.Set(float64(m.activeDrainCount))
}

type drainHolder struct {
	ctx         context.Context
	cancel      func()
	drainWriter egress.Writer
}

func newDrainHolder() drainHolder {
	ctx, cancel := context.WithCancel(context.Background())
	return drainHolder{
		ctx:         ctx,
		cancel:      cancel,
		drainWriter: nil,
	}
}
