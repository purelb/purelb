package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	group      = "spike.egress.purelb.io"
	gv         = "/apis/" + group + "/v1"
	nsPureLB   = "purelb-system"
	nsLoad     = "egress-load"
	leaseLabel = "spike.purelb.io/lease"
)

// client wraps a client-go RESTClient. Each client has its own client-go
// token-bucket rate limiter built from QPS/Burst, exactly as a typed
// clientset would (rest.RESTClientFor creates one per client).
type client struct {
	rc *rest.RESTClient
}

func adminConfig(kubeconfig string) (*rest.Config, error) {
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func newClient(base *rest.Config, qps float32, burst int) (*client, error) {
	cfg := rest.CopyConfig(base)
	cfg.QPS = qps
	cfg.Burst = burst
	cfg.RateLimiter = nil
	cfg.GroupVersion = &schema.GroupVersion{}
	cfg.APIPath = "/"
	cfg.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	cfg.ContentType = "application/json"
	cfg.AcceptContentTypes = "application/json"
	cfg.Timeout = 0
	rc, err := rest.RESTClientFor(cfg)
	if err != nil {
		return nil, err
	}
	return &client{rc: rc}, nil
}

// tokenConfig returns a config that authenticates as a service account
// bearer token, so requests are classified by API Priority and Fairness as
// ordinary workload traffic (not system:masters, which is exempt).
func tokenConfig(base *rest.Config, token string) *rest.Config {
	cfg := &rest.Config{
		Host:        base.Host,
		BearerToken: token,
		TLSClientConfig: rest.TLSClientConfig{
			CAFile: base.TLSClientConfig.CAFile,
			CAData: base.TLSClientConfig.CAData,
		},
	}
	return cfg
}

func (c *client) get(ctx context.Context, path string) ([]byte, error) {
	return c.rc.Get().AbsPath(path).Do(ctx).Raw()
}

func (c *client) post(ctx context.Context, path string, body []byte) ([]byte, error) {
	return c.rc.Post().AbsPath(path).Body(body).Do(ctx).Raw()
}

func (c *client) mergePatch(ctx context.Context, path string, body []byte) ([]byte, error) {
	return c.rc.Patch(types.MergePatchType).AbsPath(path).Body(body).Do(ctx).Raw()
}

func (c *client) delete(ctx context.Context, path string, body []byte) error {
	r := c.rc.Delete().AbsPath(path)
	if body != nil {
		r = r.Body(body)
	}
	return r.Do(ctx).Error()
}

// deleteCollection removes every object at path (a collection URL).
func (c *client) deleteCollection(ctx context.Context, path, labelSelector string) error {
	r := c.rc.Delete().AbsPath(path)
	if labelSelector != "" {
		r = r.Param("labelSelector", labelSelector)
	}
	return r.Do(ctx).Error()
}

type listMeta struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
		Continue        string `json:"continue"`
	} `json:"metadata"`
	Items []json.RawMessage `json:"items"`
}

// listAll pages through a collection.
func (c *client) listAll(ctx context.Context, path string, params map[string]string) ([]json.RawMessage, string, error) {
	var items []json.RawMessage
	cont := ""
	for {
		r := c.rc.Get().AbsPath(path).Param("limit", "2000")
		for k, v := range params {
			r = r.Param(k, v)
		}
		if cont != "" {
			r = r.Param("continue", cont)
		}
		raw, err := r.Do(ctx).Raw()
		if err != nil {
			return nil, "", err
		}
		var lm listMeta
		if err := json.Unmarshal(raw, &lm); err != nil {
			return nil, "", err
		}
		items = append(items, lm.Items...)
		if lm.Metadata.Continue == "" {
			return items, lm.Metadata.ResourceVersion, nil
		}
		cont = lm.Metadata.Continue
	}
}

type watchEvent struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

type rvOnly struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Code int `json:"code"`
}

// watchLoop is a minimal reflector: list, then watch from the list's
// resourceVersion, re-watching on stream end and re-listing on 410. It
// decodes nothing beyond the event envelope; onList/onEvent get raw objects.
// rxBytes counts the raw object bytes this watcher received.
func (c *client) watchLoop(ctx context.Context, path string, params map[string]string,
	onList func([]json.RawMessage), onEvent func(typ string, obj json.RawMessage), rxBytes *atomic.Int64) {
	for ctx.Err() == nil {
		items, rv, err := c.listAll(ctx, path, params)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logf("list %s: %v", path, err)
			time.Sleep(time.Second)
			continue
		}
		onList(items)
		relist := false
		for ctx.Err() == nil && !relist {
			r := c.rc.Get().AbsPath(path).Param("watch", "true").
				Param("resourceVersion", rv).Param("allowWatchBookmarks", "true").
				Param("timeoutSeconds", "3600")
			for k, v := range params {
				r = r.Param(k, v)
			}
			stream, err := r.Stream(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				logf("watch %s: %v", path, err)
				time.Sleep(time.Second)
				relist = true
				break
			}
			dec := json.NewDecoder(bufio.NewReaderSize(stream, 256<<10))
			for {
				var ev watchEvent
				if err := dec.Decode(&ev); err != nil {
					if err != io.EOF && ctx.Err() == nil && !strings.Contains(err.Error(), "context canceled") {
						logf("watch decode %s: %v", path, err)
					}
					break
				}
				var m rvOnly
				_ = json.Unmarshal(ev.Object, &m)
				if ev.Type == "ERROR" {
					if m.Code == http.StatusGone {
						relist = true
					}
					break
				}
				if m.Metadata.ResourceVersion != "" {
					rv = m.Metadata.ResourceVersion
				}
				if ev.Type == "BOOKMARK" {
					continue
				}
				if rxBytes != nil {
					rxBytes.Add(int64(len(ev.Object)))
				}
				onEvent(ev.Type, ev.Object)
			}
			stream.Close()
		}
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func compact(b []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, b); err != nil {
		return b
	}
	return buf.Bytes()
}

var logStart = time.Now()

func logf(format string, args ...any) {
	fmt.Printf("[%7.1fs] %s\n", time.Since(logStart).Seconds(), fmt.Sprintf(format, args...))
}
