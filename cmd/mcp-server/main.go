/*
Copyright 2026 Chronos project and its authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command chronos-mcp serves the Chronos timeline to AI agents over MCP.
//
// It runs separately from the operator on purpose. The operator holds broad
// write permissions in order to capture and revert changes; this reads the
// timeline and nothing else. Keeping them apart means an agent's access is
// bounded by this ServiceAccount's RBAC rather than by the operator's.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	chronosv1alpha1 "github.com/v1k0d3n/chronos/api/v1alpha1"
	"github.com/v1k0d3n/chronos/internal/mcpserver"
)

var scheme = runtime.NewScheme()

// stringSlice collects a repeatable string flag.
type stringSlice []string

func (s *stringSlice) String() string     { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(chronosv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		addr          string
		stdio         bool
		devLog        bool
		shutdown      time.Duration
		audiences     stringSlice
		allowUnscoped bool
		insecureHTTP  bool
		certPath      string
		certName      string
		keyName       string
	)
	flag.StringVar(&addr, "address", ":8443", "Address to serve MCP over streamable HTTP.")
	flag.StringVar(&certPath, "tls-cert-path", "", "Directory holding the serving certificate and key. "+
		"Required unless --insecure-http is set: callers send bearer tokens, and those must not cross the network in the clear.")
	flag.StringVar(&certName, "tls-cert-name", "tls.crt", "Certificate file name within --tls-cert-path.")
	flag.StringVar(&keyName, "tls-key-name", "tls.key", "Key file name within --tls-cert-path.")
	flag.BoolVar(&insecureHTTP, "insecure-http", false, "Serve plain HTTP. For local development only.")
	flag.BoolVar(&allowUnscoped, "allow-unscoped-tokens", false,
		"With --token-audience set, also accept tokens that are valid for the API server itself (such as `oc whoami -t`). "+
			"Such a token is a cluster credential; handing it to this server means trusting this server with it.")
	flag.BoolVar(&stdio, "stdio", false, "Serve over stdio instead of HTTP, for a local client.")
	flag.BoolVar(&devLog, "dev", false, "Human-readable development logging.")
	flag.DurationVar(&shutdown, "shutdown-timeout", 10*time.Second, "Grace period for in-flight requests.")
	flag.Var(&audiences, "token-audience",
		"Audience caller tokens must be minted for; repeatable. Callers then use tokens that are worthless anywhere "+
			"else (`oc create token <sa> --audience=<this>`), and the server acts as them by impersonation. "+
			"Unset, callers must present a general API server token, which the server uses directly.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	opts.Development = opts.Development || devLog

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("chronos-mcp")

	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error(err, "no Kubernetes configuration available")
		os.Exit(1)
	}

	// A plain client rather than a cached one: this process is a query front end
	// with no controllers, and a cache would hold every ChangeEvent in the
	// cluster in memory for no benefit.
	//
	// This client uses the process's own ServiceAccount. It is used only to
	// review callers' tokens — never to answer a query.
	selfClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "unable to build a Kubernetes client")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if stdio {
		// Over stdio the process already runs as the person who launched it, so
		// the ambient kubeconfig is that person's own credentials and the API
		// server applies their RBAC. There is no second identity to reconcile.
		log.Info("serving MCP over stdio", "identity", "ambient kubeconfig")
		srv := &mcpserver.Server{Provider: &mcpserver.StaticClientProvider{
			Client: selfClient,
			Reason: "ambient kubeconfig (stdio)",
		}}
		if err := mcpserver.RunStdio(ctx, srv); err != nil && !errors.Is(err, context.Canceled) {
			log.Error(err, "stdio server failed")
			os.Exit(1)
		}
		return
	}

	// Over HTTP the caller is a stranger. Build a RESTMapper once and share it,
	// so constructing a per-caller client is cheap.
	//
	// The mapper only performs discovery — it maps kinds to resources, which is
	// identical for every caller — so it is safe to build from the server's own
	// configuration and reuse. Nothing a caller can read passes through it.
	discoveryClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		log.Error(err, "unable to build an HTTP client for discovery")
		os.Exit(1)
	}
	mapper, err := apiutil.NewDynamicRESTMapper(cfg, discoveryClient)
	if err != nil {
		log.Error(err, "unable to build a REST mapper")
		os.Exit(1)
	}
	var provider mcpserver.ClientProvider
	if len(audiences) > 0 {
		provider = &mcpserver.ImpersonatingClientProvider{Base: cfg, Scheme: scheme, Mapper: mapper}
	} else {
		log.Info("no --token-audience: callers must present general API server tokens, which are cluster credentials",
			"hint", "set --token-audience so callers can use tokens that are good for nothing but this server")
		provider = &mcpserver.TokenClientProvider{Base: cfg, Scheme: scheme, Mapper: mapper}
	}
	srv := &mcpserver.Server{Provider: provider}
	verifier := mcpserver.NewTokenReviewVerifier(selfClient, audiences, allowUnscoped)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcpserver.NewHTTPHandler(srv, verifier))
	// Distinct from /mcp so a probe cannot be mistaken for a session, and so a
	// readiness check does not create protocol state.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No ReadTimeout or WriteTimeout: streamable HTTP keeps a response
		// open for as long as a session lasts. Idle connections are reaped.
		IdleTimeout: 2 * time.Minute,
	}

	// Callers put a bearer token on every request. Over plain HTTP that token
	// crosses the pod network readable by anything on the path, so TLS is the
	// default and plain HTTP has to be asked for by name.
	var serve func() error
	switch {
	case certPath != "":
		watcher, err := certwatcher.New(filepath.Join(certPath, certName), filepath.Join(certPath, keyName))
		if err != nil {
			log.Error(err, "unable to load the serving certificate", "path", certPath)
			os.Exit(1)
		}
		go func() {
			if err := watcher.Start(ctx); err != nil {
				log.Error(err, "certificate watcher stopped")
			}
		}()
		httpSrv.TLSConfig = &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: watcher.GetCertificate,
		}
		serve = func() error { return httpSrv.ListenAndServeTLS("", "") }
	case insecureHTTP:
		log.Info("serving PLAIN HTTP: bearer tokens will cross the network unencrypted", "address", addr)
		serve = httpSrv.ListenAndServe
	default:
		log.Error(nil, "refusing to serve without TLS: set --tls-cert-path, or --insecure-http for local development")
		os.Exit(1)
	}

	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdown)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Error(err, "graceful shutdown failed")
		}
	}()

	log.Info("serving MCP over streamable HTTP",
		"address", addr, "path", "/mcp", "tls", certPath != "", "identity", provider.Describe(), "audiences", audiences.String())
	if err := serve(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error(err, "server failed")
		os.Exit(1)
	}
}
