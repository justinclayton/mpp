package router

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/matt-deboer/mpp/pkg/locator"
	"github.com/matt-deboer/mpp/pkg/selector"
	"github.com/matt-deboer/mpp/pkg/version"
	"github.com/vulcand/oxy/buffer"
	"github.com/vulcand/oxy/forward"
)

// Router provides dynamic routing of http requests based on a configurable strategy
type Router struct {
	locators        []locator.Locator
	selector        *selector.Selector
	selection       *selector.Result
	forward         http.Handler
	buffer          *buffer.Buffer
	rewriter        urlRewriter
	affinityOptions []AffinityOption
	interval        time.Duration
	metrics         *metrics
	// used to mark control of the selection process
	theConch            chan struct{}
	selectionInProgress sync.RWMutex
}

// Status contains a snapshot status summary of the router state
type Status struct {
	Endpoints           []*locator.PrometheusEndpoint
	Strategy            string
	StrategyDescription string
	AffinityOptions     string
	ComparisonMetric    string
	Interval            time.Duration
}

type urlRewriter func(u *url.URL)

// NewRouter constructs a new router based on the provided stategy and locators
func NewRouter(interval time.Duration, affinityOptions []AffinityOption,
	locators []locator.Locator, strategyArgs ...string) (*Router, error) {

	sel, err := selector.NewSelector(locators, strategyArgs...)
	if err != nil {
		return nil, err
	}

	r := &Router{
		locators:        locators,
		selector:        sel,
		affinityOptions: affinityOptions,
		interval:        interval,
		metrics:         newMetrics(version.Name),
		theConch:        make(chan struct{}, 1),
	}

	// Set up the lock
	r.theConch <- struct{}{}
	r.doSelection()
	go func() {
		// TODO: create shutdown channel for this
		for {
			if log.GetLevel() >= log.DebugLevel {
				log.Debugf("Backend selection is sleeping for %s", interval)
			}
			time.Sleep(r.interval)
			r.doSelection()
		}
	}()

	r.forward, _ = forward.New()
	r.buffer, _ = buffer.New(&internalRouter{
		router:   r,
		affinity: newAffinityProvider(affinityOptions),
	},
		buffer.Retry(`IsNetworkError() && Attempts() < 2`))
	return r, nil
}

func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.buffer.ServeHTTP(w, retryableRequest(req))
}

func (r *Router) doSelection() {
	select {
	case _ = <-r.theConch:
		r.selectionInProgress.Lock()
		defer r.selectionInProgress.Unlock()
		if log.GetLevel() >= log.DebugLevel {
			log.Debugf("Got selection lock; performing selection")
		}

		result, err := r.selector.Select()

		if result.Selection == nil || len(result.Selection) == 0 {
			if err != nil {
				log.Errorf("Selector returned no valid selection, and error: %v", err)
				if r.selection == nil {
					r.selection = result
				}
			} else {
				r.selection = result
				log.Warnf("Selector returned no valid selection")
			}
		} else {
			if log.GetLevel() >= log.DebugLevel {
				log.Debugf("Selected targets: %v", result.Selection)
			}
			if r.selection == nil || !equal(r.selection.Selection, result.Selection) {
				log.Infof("New targets differ from current selection %v; updating rewriter => %v", r.selection, result)
				r.rewriter = func(u *url.URL) {
					selection := result.Selection
					i := r.selector.Strategy.NextIndex(selection)
					target := selection[i]
					u.Host = target.Host
					u.Scheme = target.Scheme
				}
			} else if log.GetLevel() >= log.DebugLevel {
				log.Debugf("Selection is unchanged: %v", r.selection)
			}
			r.selection = result
		}

		r.metrics.selectedBackends.Set(float64(len(result.Selection)))
		r.metrics.selectionEvents.Inc()
		if log.GetLevel() >= log.DebugLevel {
			log.Debugf("Returning selection lock")
		}
		r.theConch <- struct{}{}
	default:
		if log.GetLevel() >= log.DebugLevel {
			log.Debugf("Selection is already in-progress; awaiting result")
		}
		r.selectionInProgress.RLock()
		defer r.selectionInProgress.RUnlock()
	}
}

func equal(a, b []*url.URL) bool {
	for i, v := range a {
		if *v != *b[i] {
			return false
		}
	}
	return len(a) == len(b)
}

func contains(a []*url.URL, u *url.URL) bool {
	for _, v := range a {
		if *u == *v {
			return true
		}
	}
	return false
}

func backend(u *url.URL) string {
	return fmt.Sprintf("%s://%s", u.Scheme, u.Host)
}

// Status returns a summary of the router's current state
func (r *Router) Status() *Status {
	return &Status{
		Endpoints:           r.selection.Candidates,
		Strategy:            r.selector.Strategy.Name(),
		StrategyDescription: r.selector.Strategy.Description(),
		ComparisonMetric:    r.selector.Strategy.ComparisonMetricName(),
		AffinityOptions:     strings.Trim(fmt.Sprintf("%v", r.affinityOptions), "[]"),
		Interval:            r.interval,
	}
}
