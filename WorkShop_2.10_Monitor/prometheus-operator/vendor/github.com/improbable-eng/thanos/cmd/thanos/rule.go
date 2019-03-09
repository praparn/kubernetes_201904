package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/improbable-eng/thanos/pkg/extprom"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/improbable-eng/thanos/pkg/alert"
	"github.com/improbable-eng/thanos/pkg/block"
	"github.com/improbable-eng/thanos/pkg/cluster"
	"github.com/improbable-eng/thanos/pkg/discovery/cache"
	"github.com/improbable-eng/thanos/pkg/discovery/dns"
	"github.com/improbable-eng/thanos/pkg/objstore/client"
	"github.com/improbable-eng/thanos/pkg/runutil"
	"github.com/improbable-eng/thanos/pkg/shipper"
	"github.com/improbable-eng/thanos/pkg/store"
	"github.com/improbable-eng/thanos/pkg/store/storepb"
	"github.com/improbable-eng/thanos/pkg/tracing"
	"github.com/improbable-eng/thanos/pkg/ui"
	"github.com/oklog/run"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/common/route"
	"github.com/prometheus/prometheus/discovery/file"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	promlabels "github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/rules"
	"github.com/prometheus/prometheus/storage/tsdb"
	"github.com/prometheus/prometheus/util/strutil"
	"github.com/prometheus/tsdb/labels"
	"google.golang.org/grpc"
	"gopkg.in/alecthomas/kingpin.v2"
)

// registerRule registers a rule command.
func registerRule(m map[string]setupFunc, app *kingpin.Application, name string) {
	cmd := app.Command(name, "ruler evaluating Prometheus rules against given Query nodes, exposing Store API and storing old blocks in bucket")

	grpcBindAddr, httpBindAddr, cert, key, clientCA, newPeerFn := regCommonServerFlags(cmd)

	labelStrs := cmd.Flag("label", "Labels to be applied to all generated metrics (repeated). Similar to external labels for Prometheus, used to identify ruler and its blocks as unique source.").
		PlaceHolder("<name>=\"<value>\"").Strings()

	dataDir := cmd.Flag("data-dir", "data directory").Default("data/").String()

	ruleFiles := cmd.Flag("rule-file", "Rule files that should be used by rule manager. Can be in glob format (repeated).").
		Default("rules/").Strings()

	evalInterval := modelDuration(cmd.Flag("eval-interval", "The default evaluation interval to use.").
		Default("30s"))
	tsdbBlockDuration := modelDuration(cmd.Flag("tsdb.block-duration", "Block duration for TSDB block.").
		Default("2h"))
	tsdbRetention := modelDuration(cmd.Flag("tsdb.retention", "Block retention time on local disk.").
		Default("48h"))

	alertmgrs := cmd.Flag("alertmanagers.url", "Alertmanager replica URLs to push firing alerts. Ruler claims success if push to at least one alertmanager from discovered succeeds. The scheme may be prefixed with 'dns+' or 'dnssrv+' to detect Alertmanager IPs through respective DNS lookups. The port defaults to 9093 or the SRV record's value. The URL path is used as a prefix for the regular Alertmanager API path.").
		Strings()

	alertmgrsTimeout := cmd.Flag("alertmanagers.send-timeout", "Timeout for sending alerts to alertmanager").Default("10s").Duration()

	alertQueryURL := cmd.Flag("alert.query-url", "The external Thanos Query URL that would be set in all alerts 'Source' field").String()

	alertExcludeLabels := cmd.Flag("alert.label-drop", "Labels by name to drop before sending to alertmanager. This allows alert to be deduplicated on replica label (repeated). Similar Prometheus alert relabelling").
		Strings()

	objStoreConfig := regCommonObjStoreFlags(cmd, "")

	queries := cmd.Flag("query", "Addresses of statically configured query API servers (repeatable). The scheme may be prefixed with 'dns+' or 'dnssrv+' to detect query API servers through respective DNS lookups.").
		PlaceHolder("<query>").Strings()

	fileSDFiles := cmd.Flag("query.sd-files", "Path to file that contain addresses of query peers. The path can be a glob pattern (repeatable).").
		PlaceHolder("<path>").Strings()

	fileSDInterval := modelDuration(cmd.Flag("query.sd-interval", "Refresh interval to re-read file SD files. (used as a fallback)").
		Default("5m"))

	dnsSDInterval := modelDuration(cmd.Flag("query.sd-dns-interval", "Interval between DNS resolutions.").
		Default("30s"))

	m[name] = func(g *run.Group, logger log.Logger, reg *prometheus.Registry, tracer opentracing.Tracer, _ bool) error {
		lset, err := parseFlagLabels(*labelStrs)
		if err != nil {
			return errors.Wrap(err, "parse labels")
		}
		peer, err := newPeerFn(logger, reg, false, "", false)
		if err != nil {
			return errors.Wrap(err, "new cluster peer")
		}
		alertQueryURL, err := url.Parse(*alertQueryURL)
		if err != nil {
			return errors.Wrap(err, "parse alert query url")
		}

		tsdbOpts := &tsdb.Options{
			MinBlockDuration: *tsdbBlockDuration,
			MaxBlockDuration: *tsdbBlockDuration,
			Retention:        *tsdbRetention,
			NoLockfile:       true,
			WALFlushInterval: 30 * time.Second,
		}

		lookupQueries := map[string]struct{}{}
		for _, q := range *queries {
			if _, ok := lookupQueries[q]; ok {
				return errors.Errorf("Address %s is duplicated for --query flag.", q)
			}

			lookupQueries[q] = struct{}{}
		}

		var fileSD *file.Discovery
		if len(*fileSDFiles) > 0 {
			conf := &file.SDConfig{
				Files:           *fileSDFiles,
				RefreshInterval: *fileSDInterval,
			}
			fileSD = file.NewDiscovery(conf, logger)
		}

		return runRule(g,
			logger,
			reg,
			tracer,
			lset,
			*alertmgrs,
			*alertmgrsTimeout,
			*grpcBindAddr,
			*cert,
			*key,
			*clientCA,
			*httpBindAddr,
			time.Duration(*evalInterval),
			*dataDir,
			*ruleFiles,
			peer,
			objStoreConfig,
			tsdbOpts,
			name,
			alertQueryURL,
			*alertExcludeLabels,
			*queries,
			fileSD,
			time.Duration(*dnsSDInterval),
		)
	}
}

// runRule runs a rule evaluation component that continuously evaluates alerting and recording
// rules. It sends alert notifications and writes TSDB data for results like a regular Prometheus server.
func runRule(
	g *run.Group,
	logger log.Logger,
	reg *prometheus.Registry,
	tracer opentracing.Tracer,
	lset labels.Labels,
	alertmgrURLs []string,
	alertmgrsTimeout time.Duration,
	grpcBindAddr string,
	cert string,
	key string,
	clientCA string,
	httpBindAddr string,
	evalInterval time.Duration,
	dataDir string,
	ruleFiles []string,
	peer cluster.Peer,
	objStoreConfig *pathOrContent,
	tsdbOpts *tsdb.Options,
	component string,
	alertQueryURL *url.URL,
	alertExcludeLabels []string,
	queryAddrs []string,
	fileSD *file.Discovery,
	dnsSDInterval time.Duration,
) error {
	configSuccess := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "thanos_rule_config_last_reload_successful",
		Help: "Whether the last configuration reload attempt was successful.",
	})
	configSuccessTime := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "thanos_rule_config_last_reload_success_timestamp_seconds",
		Help: "Timestamp of the last successful configuration reload.",
	})
	duplicatedQuery := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "thanos_rule_duplicated_query_address",
		Help: "The number of times a duplicated query addresses is detected from the different configs in rule",
	})
	alertMngrAddrResolutionErrors := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "thanos_rule_alertmanager_address_resolution_errors",
		Help: "The number of times resolving an address of an alertmanager has failed inside Thanos Rule",
	})
	rulesLoaded := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "thanos_rule_loaded_rules",
			Help: "Loaded rules partitioned by file and group",
		},
		[]string{"file", "group"},
	)
	reg.MustRegister(configSuccess)
	reg.MustRegister(configSuccessTime)
	reg.MustRegister(duplicatedQuery)
	reg.MustRegister(alertMngrAddrResolutionErrors)
	reg.MustRegister(rulesLoaded)

	for _, addr := range queryAddrs {
		if addr == "" {
			return errors.New("static querier address cannot be empty")
		}
	}

	db, err := tsdb.Open(dataDir, log.With(logger, "component", "tsdb"), reg, tsdbOpts)
	if err != nil {
		return errors.Wrap(err, "open TSDB")
	}
	{
		done := make(chan struct{})
		g.Add(func() error {
			<-done
			return db.Close()
		}, func(error) {
			close(done)
		})
	}

	// FileSD query addresses.
	fileSDCache := cache.New()
	dnsProvider := dns.NewProvider(logger, extprom.NewSubsystem(reg, "rule_query"))

	// Hit the HTTP query API of query peers in randomized order until we get a result
	// back or the context get canceled.
	queryFn := func(ctx context.Context, q string, t time.Time) (promql.Vector, error) {
		var addrs []string

		// Add addresses from gossip.
		peers := peer.PeerStates(cluster.PeerTypeQuery)
		var ids []string
		for id := range peers {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i int, j int) bool {
			return strings.Compare(ids[i], ids[j]) < 0
		})
		for _, id := range ids {
			addrs = append(addrs, peers[id].QueryAPIAddr)
		}

		// Add DNS resolved addresses from static flags and file SD.
		addrs = append(addrs, dnsProvider.Addresses()...)

		removeDuplicateQueryAddrs(logger, duplicatedQuery, addrs)

		for _, i := range rand.Perm(len(addrs)) {
			vec, err := queryPrometheusInstant(ctx, logger, addrs[i], q, t)
			if err != nil {
				return nil, err
			}
			return vec, nil
		}
		return nil, errors.Errorf("no query peer reachable")
	}

	// Run rule evaluation and alert notifications.
	var (
		alertmgrs = newAlertmanagerSet(alertmgrURLs)
		alertQ    = alert.NewQueue(logger, reg, 10000, 100, labelsTSDBToProm(lset), alertExcludeLabels)
		mgr       *rules.Manager
	)
	{
		ctx, cancel := context.WithCancel(context.Background())
		ctx = tracing.ContextWithTracer(ctx, tracer)

		notify := func(ctx context.Context, expr string, alerts ...*rules.Alert) error {
			res := make([]*alert.Alert, 0, len(alerts))
			for _, alrt := range alerts {
				// Only send actually firing alerts.
				if alrt.State == rules.StatePending {
					continue
				}
				a := &alert.Alert{
					StartsAt:     alrt.FiredAt,
					Labels:       alrt.Labels,
					Annotations:  alrt.Annotations,
					GeneratorURL: alertQueryURL.String() + strutil.TableLinkForExpression(expr),
				}
				if !alrt.ResolvedAt.IsZero() {
					a.EndsAt = alrt.ResolvedAt
				}
				res = append(res, a)
			}
			alertQ.Push(res)

			return nil
		}
		mgr = rules.NewManager(&rules.ManagerOptions{
			Context:     ctx,
			QueryFunc:   queryFn,
			NotifyFunc:  notify,
			Logger:      log.With(logger, "component", "rules"),
			Appendable:  tsdb.Adapter(db, 0),
			Registerer:  reg,
			ExternalURL: nil,
		})
		g.Add(func() error {
			mgr.Run()
			<-ctx.Done()
			mgr.Stop()
			return nil
		}, func(error) {
			cancel()
		})
	}
	{
		var storeLset []storepb.Label
		for _, l := range lset {
			storeLset = append(storeLset, storepb.Label{Name: l.Name, Value: l.Value})
		}

		ctx, cancel := context.WithCancel(context.Background())
		g.Add(func() error {
			// New gossip cluster.
			if err = peer.Join(cluster.PeerTypeSource, cluster.PeerMetadata{
				Labels: storeLset,
				// Start out with the full time range. The shipper will constrain it later.
				// TODO(fabxc): minimum timestamp is never adjusted if shipping is disabled.
				MinTime: 0,
				MaxTime: math.MaxInt64,
			}); err != nil {
				return errors.Wrap(err, "join cluster")
			}

			<-ctx.Done()
			return nil
		}, func(error) {
			cancel()
			peer.Close(5 * time.Second)
		})
	}
	{
		// TODO(bwplotka): https://github.com/improbable-eng/thanos/issues/660
		sdr := alert.NewSender(logger, reg, alertmgrs.get, nil, alertmgrsTimeout)
		ctx, cancel := context.WithCancel(context.Background())

		g.Add(func() error {
			for {
				sdr.Send(ctx, alertQ.Pop(ctx.Done()))

				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
			}
		}, func(error) {
			cancel()
		})
	}
	{
		ctx, cancel := context.WithCancel(context.Background())

		g.Add(func() error {
			return runutil.Repeat(30*time.Second, ctx.Done(), func() error {
				if err := alertmgrs.update(ctx); err != nil {
					level.Error(logger).Log("msg", "refreshing alertmanagers failed", "err", err)
					alertMngrAddrResolutionErrors.Inc()
				}
				return nil
			})
		}, func(error) {
			cancel()
		})
	}
	// Run File Service Discovery and update the query addresses when the files are modified
	if fileSD != nil {
		var fileSDUpdates chan []*targetgroup.Group
		ctxRun, cancelRun := context.WithCancel(context.Background())

		fileSDUpdates = make(chan []*targetgroup.Group)

		g.Add(func() error {
			fileSD.Run(ctxRun, fileSDUpdates)
			return nil
		}, func(error) {
			cancelRun()
		})

		ctxUpdate, cancelUpdate := context.WithCancel(context.Background())
		g.Add(func() error {
			for {
				select {
				case update := <-fileSDUpdates:
					// Discoverers sometimes send nil updates so need to check for it to avoid panics
					if update == nil {
						continue
					}
					fileSDCache.Update(update)
				case <-ctxUpdate.Done():
					return nil
				}
			}
		}, func(error) {
			cancelUpdate()
			close(fileSDUpdates)
		})
	}

	// Handle reload and termination interrupts.
	reload := make(chan struct{}, 1)
	{
		cancel := make(chan struct{})
		reload <- struct{}{} // initial reload

		g.Add(func() error {
			for {
				select {
				case <-cancel:
					return errors.New("canceled")
				case <-reload:
				}

				level.Debug(logger).Log("msg", "configured rule files", "files", strings.Join(ruleFiles, ","))
				var files []string
				for _, pat := range ruleFiles {
					fs, err := filepath.Glob(pat)
					if err != nil {
						// The only error can be a bad pattern.
						level.Error(logger).Log("msg", "retrieving rule files failed. Ignoring file.", "pattern", pat, "err", err)
						continue
					}
					files = append(files, fs...)
				}

				level.Info(logger).Log("msg", "reload rule files", "numFiles", len(files))
				if err := mgr.Update(evalInterval, files); err != nil {
					configSuccess.Set(0)
					level.Error(logger).Log("msg", "reloading rules failed", "err", err)
					continue
				}

				configSuccess.Set(1)
				configSuccessTime.Set(float64(time.Now().UnixNano()) / 1e9)

				rulesLoaded.Reset()
				for _, group := range mgr.RuleGroups() {
					rulesLoaded.WithLabelValues(group.File(), group.Name()).Set(float64(len(group.Rules())))
				}
			}
		}, func(error) {
			close(cancel)
		})
	}
	{
		cancel := make(chan struct{})

		g.Add(func() error {
			c := make(chan os.Signal, 1)
			for {
				signal.Notify(c, syscall.SIGHUP)
				select {
				case <-c:
					select {
					case reload <- struct{}{}:
					default:
					}
				case <-cancel:
					return errors.New("canceled")
				}
			}
		}, func(error) {
			close(cancel)
		})
	}
	// Periodically update the addresses from static flags and file SD by resolving them using DNS SD if necessary.
	{
		ctx, cancel := context.WithCancel(context.Background())
		g.Add(func() error {
			return runutil.Repeat(dnsSDInterval, ctx.Done(), func() error {
				dnsProvider.Resolve(ctx, append(fileSDCache.Addresses(), queryAddrs...))
				return nil
			})
		}, func(error) {
			cancel()
		})
	}
	// Start gRPC server.
	{
		l, err := net.Listen("tcp", grpcBindAddr)
		if err != nil {
			return errors.Wrap(err, "listen API address")
		}
		logger := log.With(logger, "component", "store")

		store := store.NewTSDBStore(logger, reg, db, lset)

		opts, err := defaultGRPCServerOpts(logger, reg, tracer, cert, key, clientCA)
		if err != nil {
			return errors.Wrap(err, "setup gRPC options")
		}
		s := grpc.NewServer(opts...)
		storepb.RegisterStoreServer(s, store)

		g.Add(func() error {
			return errors.Wrap(s.Serve(l), "serve gRPC")
		}, func(error) {
			s.Stop()
			runutil.CloseWithLogOnErr(logger, l, "store gRPC listener")
		})
	}
	// Start UI & metrics HTTP server.
	{
		router := route.New()
		router.Post("/-/reload", func(w http.ResponseWriter, r *http.Request) {
			reload <- struct{}{}
		})

		ui.NewRuleUI(logger, mgr, alertQueryURL.String()).Register(router)

		mux := http.NewServeMux()
		registerMetrics(mux, reg)
		registerProfile(mux)
		mux.Handle("/", router)

		l, err := net.Listen("tcp", httpBindAddr)
		if err != nil {
			return errors.Wrapf(err, "listen HTTP on address %s", httpBindAddr)
		}

		g.Add(func() error {
			level.Info(logger).Log("msg", "Listening for ui requests", "address", httpBindAddr)
			return errors.Wrap(http.Serve(l, mux), "serve query")
		}, func(error) {
			runutil.CloseWithLogOnErr(logger, l, "query and metric listener")
		})
	}

	var uploads = true

	bucketConfig, err := objStoreConfig.Content()
	if err != nil {
		return err
	}
	// The background shipper continuously scans the data directory and uploads
	// new blocks to Google Cloud Storage or an S3-compatible storage service.
	bkt, err := client.NewBucket(logger, bucketConfig, reg, component)
	if err != nil && err != client.ErrNotFound {
		return err
	}

	if err == client.ErrNotFound {
		level.Info(logger).Log("msg", "No supported bucket was configured, uploads will be disabled")
		uploads = false
	}

	if uploads {
		// Ensure we close up everything properly.
		defer func() {
			if err != nil {
				runutil.CloseWithLogOnErr(logger, bkt, "bucket client")
			}
		}()

		s := shipper.New(logger, nil, dataDir, bkt, func() labels.Labels { return lset }, block.RulerSource)

		ctx, cancel := context.WithCancel(context.Background())

		g.Add(func() error {
			defer runutil.CloseWithLogOnErr(logger, bkt, "bucket client")

			return runutil.Repeat(30*time.Second, ctx.Done(), func() error {
				s.Sync(ctx)

				minTime, _, err := s.Timestamps()
				if err != nil {
					level.Warn(logger).Log("msg", "reading timestamps failed", "err", err)
				} else {
					peer.SetTimestamps(minTime, math.MaxInt64)
				}
				return nil
			})
		}, func(error) {
			cancel()
		})
	}

	level.Info(logger).Log("msg", "starting rule node", "peer", peer.Name())
	return nil
}

// Scalar response consists of array with mixed types so it needs to be
// unmarshaled separatelly.
func convertScalarJSONToVector(scalarJSONResult json.RawMessage) (model.Vector, error) {
	var (
		// Do not specify exact length of the expected slice since JSON unmarshaling
		// would make the leght fit the size and we won't be able to check the length afterwards.
		resultPointSlice []json.RawMessage
		resultTime       model.Time
		resultValue      model.SampleValue
	)
	if err := json.Unmarshal(scalarJSONResult, &resultPointSlice); err != nil {
		return nil, err
	}
	if len(resultPointSlice) != 2 {
		return nil, errors.Errorf("invalid scalar result format %v, expected timestamp -> value tuple", resultPointSlice)
	}
	if err := json.Unmarshal(resultPointSlice[0], &resultTime); err != nil {
		return nil, errors.Wrapf(err, "unmarshaling scalar time from %v", resultPointSlice)
	}
	if err := json.Unmarshal(resultPointSlice[1], &resultValue); err != nil {
		return nil, errors.Wrapf(err, "unmarshaling scalar value from %v", resultPointSlice)
	}
	return model.Vector{&model.Sample{
		Metric:    model.Metric{},
		Value:     resultValue,
		Timestamp: resultTime}}, nil
}

func queryPrometheusInstant(ctx context.Context, logger log.Logger, addr, query string, t time.Time) (promql.Vector, error) {
	u, err := url.Parse(fmt.Sprintf("http://%s/api/v1/query", addr))
	if err != nil {
		return nil, err
	}
	params := url.Values{}
	params.Add("query", query)
	params.Add("time", t.Format(time.RFC3339Nano))
	params.Add("dedup", "true")
	u.RawQuery = params.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	span, ctx := tracing.StartSpan(ctx, "/rule_instant_query HTTP[client]")
	defer span.Finish()

	req = req.WithContext(ctx)

	client := &http.Client{
		Transport: tracing.HTTPTripperware(logger, http.DefaultTransport),
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer runutil.CloseWithLogOnErr(logger, resp.Body, "query body")

	// Decode only ResultType and load Result only as RawJson since we don't know
	// structure of the Result yet.
	var m struct {
		Data struct {
			ResultType string          `json:"resultType"`
			Result     json.RawMessage `json:"result"`
		} `json:"data"`
	}

	if err = json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}

	var vectorResult model.Vector

	// Decode the Result depending on the ResultType
	// Currently only `vector` and `scalar` types are supported
	switch m.Data.ResultType {
	case promql.ValueTypeVector:
		if err = json.Unmarshal(m.Data.Result, &vectorResult); err != nil {
			return nil, err
		}
	case promql.ValueTypeScalar:
		vectorResult, err = convertScalarJSONToVector(m.Data.Result)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.Errorf("unknown response type: '%q'", m.Data.ResultType)
	}

	vec := make(promql.Vector, 0, len(vectorResult))

	for _, e := range vectorResult {
		lset := make(promlabels.Labels, 0, len(e.Metric))

		for k, v := range e.Metric {
			lset = append(lset, promlabels.Label{
				Name:  string(k),
				Value: string(v),
			})
		}
		sort.Sort(lset)

		vec = append(vec, promql.Sample{
			Metric: lset,
			Point:  promql.Point{T: int64(e.Timestamp), V: float64(e.Value)},
		})
	}

	return vec, nil
}

type alertmanagerSet struct {
	resolver dns.Resolver
	addrs    []string
	mtx      sync.Mutex
	current  []*url.URL
}

func newAlertmanagerSet(addrs []string) *alertmanagerSet {
	return &alertmanagerSet{
		resolver: dns.NewResolver(),
		addrs:    addrs,
	}
}

func (s *alertmanagerSet) get() []*url.URL {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	return s.current
}

const defaultAlertmanagerPort = 9093

func (s *alertmanagerSet) update(ctx context.Context) error {
	var result []*url.URL
	for _, addr := range s.addrs {
		var (
			name           = addr
			qtype          dns.QType
			resolvedDomain []string
		)

		if nameQtype := strings.SplitN(addr, "+", 2); len(nameQtype) == 2 {
			name, qtype = nameQtype[1], dns.QType(nameQtype[0])
		}

		u, err := url.Parse(name)
		if err != nil {
			return errors.Wrapf(err, "parse URL %q", name)
		}

		// Get only the host and resolve it if needed.
		host := u.Host
		if qtype != "" {
			if qtype == dns.A {
				_, _, err = net.SplitHostPort(host)
				if err != nil {
					// The host could be missing a port. Append the defaultAlertmanagerPort.
					host = host + ":" + strconv.Itoa(defaultAlertmanagerPort)
				}
			}
			resolvedDomain, err = s.resolver.Resolve(ctx, host, qtype)
			if err != nil {
				return errors.Wrap(err, "alertmanager resolve")
			}
		} else {
			resolvedDomain = []string{host}
		}

		for _, resolved := range resolvedDomain {
			result = append(result, &url.URL{
				Scheme: u.Scheme,
				Host:   resolved,
				Path:   u.Path,
				User:   u.User,
			})
		}
	}

	s.mtx.Lock()
	s.current = result
	s.mtx.Unlock()

	return nil
}

func parseFlagLabels(s []string) (labels.Labels, error) {
	var lset labels.Labels
	for _, l := range s {
		parts := strings.SplitN(l, "=", 2)
		if len(parts) != 2 {
			return nil, errors.Errorf("unrecognized label %q", l)
		}
		val, err := strconv.Unquote(parts[1])
		if err != nil {
			return nil, errors.Wrap(err, "unquote label value")
		}
		lset = append(lset, labels.Label{Name: parts[0], Value: val})
	}
	return lset, nil
}

func labelsTSDBToProm(lset labels.Labels) (res promlabels.Labels) {
	for _, l := range lset {
		res = append(res, promlabels.Label{
			Name:  l.Name,
			Value: l.Value,
		})
	}
	return res
}

func removeDuplicateQueryAddrs(logger log.Logger, duplicatedQueriers prometheus.Counter, addrs []string) []string {
	set := make(map[string]struct{})
	for _, addr := range addrs {
		if _, ok := set[addr]; ok {
			level.Warn(logger).Log("msg", "Duplicate query address is provided - %v", addr)
			duplicatedQueriers.Inc()
		}
		set[addr] = struct{}{}
	}

	deduplicated := make([]string, 0, len(set))
	for key := range set {
		deduplicated = append(deduplicated, key)
	}
	return deduplicated
}
