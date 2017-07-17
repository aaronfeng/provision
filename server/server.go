// Package server DigitalRebar Provision Server
//
// An RestFUL API-driven Provisioner and DHCP server
//
// Terms Of Service:
//
// There are no TOS at this moment, use at your own risk we take no responsibility
//
//     Schemes: https
//     BasePath: /api/v3
//     Version: 0.1.0
//     License: APL https://raw.githubusercontent.com/digitalrebar/digitalrebar/master/LICENSE.md
//     Contact: Greg Althaus<greg@rackn.com> http://rackn.com
//
//     Security:
//       - basicAuth: []
//       - Bearer: []
//
//     Consumes:
//     - application/json
//
//     Produces:
//     - application/json
//
// swagger:meta
package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/digitalrebar/digitalrebar/go/common/client"
	"github.com/digitalrebar/digitalrebar/go/common/store"
	"github.com/digitalrebar/provision"
	"github.com/digitalrebar/provision/backend"
	"github.com/digitalrebar/provision/frontend"
	"github.com/digitalrebar/provision/midlayer"
)

type ProgOpts struct {
	VersionFlag         bool   `long:"version" description:"Print Version and exit"`
	DisableProvisioner  bool   `long:"disable-provisioner" description:"Disable provisioner"`
	DisableDHCP         bool   `long:"disable-dhcp" description:"Disable DHCP"`
	StaticPort          int    `long:"static-port" description:"Port the static HTTP file server should listen on" default:"8091"`
	TftpPort            int    `long:"tftp-port" description:"Port for the TFTP server to listen on" default:"69"`
	ApiPort             int    `long:"api-port" description:"Port for the API server to listen on" default:"8092"`
	DhcpPort            int    `long:"dhcp-port" description:"Port for the DHCP server to listen on" default:"67"`
	UnknownTokenTimeout int    `long:"unknown-token-timeout" description:"The default timeout in seconds for the machine create authorization token" default:"600"`
	KnownTokenTimeout   int    `long:"known-token-timeout" description:"The default timeout in seconds for the machine update authorization token" default:"3600"`
	BackEndType         string `long:"backend" description:"Storage backend to use. Can be either 'consul' or 'directory'" default:"directory"`
	DataRoot            string `long:"data-root" description:"Location we should store runtime information in" default:"/var/lib/dr-provision"`
	OurAddress          string `long:"static-ip" description:"IP address to advertise for the static HTTP file server" default:"192.168.124.11"`
	FileRoot            string `long:"file-root" description:"Root of filesystem we should manage" default:"/var/lib/tftpboot"`
	PluginRoot          string `long:"plugin-root" description:"Directory for plugins" default:"/var/lib/dr-provision-plugins"`
	DevUI               string `long:"dev-ui" description:"Root of UI Pages for Development"`
	DhcpInterfaces      string `long:"dhcp-ifs" description:"Comma-seperated list of interfaces to listen for DHCP packets" default:""`
	DefaultBootEnv      string `long:"default-boot-env" description:"The default bootenv for the nodes" default:"sledgehammer"`
	UnknownBootEnv      string `long:"unknown-boot-env" description:"The unknown bootenv for the system.  Should be \"ignore\" or \"discovery\"" default:"ignore"`

	DebugBootEnv  int    `long:"debug-bootenv" description:"Debug level for the BootEnv System - 0 = off, 1 = info, 2 = debug" default:"0"`
	DebugDhcp     int    `long:"debug-dhcp" description:"Debug level for the DHCP Server - 0 = off, 1 = info, 2 = debug" default:"0"`
	DebugRenderer int    `long:"debug-renderer" description:"Debug level for the Template Renderer - 0 = off, 1 = info, 2 = debug" default:"0"`
	TlsKeyFile    string `long:"tls-key" description:"The TLS Key File" default:"server.key"`
	TlsCertFile   string `long:"tls-cert" description:"The TLS Cert File" default:"server.crt"`
}

func mkdir(d string, logger *log.Logger) {
	err := os.MkdirAll(d, 0755)
	if err != nil {
		logger.Fatalf("Error creating required directory %s: %v", d, err)
	}
}

func Server(c_opts *ProgOpts) {
	var err error

	logger := log.New(os.Stderr, "dr-provision", log.LstdFlags|log.Lmicroseconds|log.LUTC)

	if c_opts.VersionFlag {
		logger.Fatalf("Version: %s", provision.RS_VERSION)
	}
	logger.Printf("Version: %s\n", provision.RS_VERSION)

	mkdir(c_opts.FileRoot, logger)

	var backendStore store.SimpleStore
	switch c_opts.BackEndType {
	case "consul":
		mkdir(c_opts.DataRoot, logger)
		consulClient, err := client.Consul(true)
		if err != nil {
			logger.Fatalf("Error talking to Consul: %v", err)
		}
		backendStore, err = store.NewSimpleConsulStore(consulClient, c_opts.DataRoot)
	case "directory":
		mkdir(c_opts.DataRoot, logger)
		backendStore, err = store.NewFileBackend(c_opts.DataRoot)
	case "memory":
		backendStore = store.NewSimpleMemoryStore()
		err = nil
	case "bolt", "local":
		mkdir(c_opts.DataRoot, logger)
		backendStore, err = store.NewSimpleLocalStore(c_opts.DataRoot)
	default:
		logger.Fatalf("Unknown storage backend type %v\n", c_opts.BackEndType)
	}
	if err != nil {
		logger.Fatalf("Error using backing store %s: %v", c_opts.BackEndType, err)
	}

	// We have a backend, now get default assets
	logger.Printf("Extracting Default Assets\n")
	if err := ExtractAssets(c_opts.FileRoot); err != nil {
		logger.Fatalf("Unable to extract assets: %v", err)
	}

	services := make([]midlayer.Service, 0, 0)
	publishers := backend.NewPublishers()

	dt := backend.NewDataTracker(backendStore,
		c_opts.FileRoot,
		c_opts.OurAddress,
		c_opts.StaticPort,
		c_opts.ApiPort,
		logger,
		map[string]string{
			"debugBootEnv":        fmt.Sprintf("%d", c_opts.DebugBootEnv),
			"debugDhcp":           fmt.Sprintf("%d", c_opts.DebugDhcp),
			"debugRenderer":       fmt.Sprintf("%d", c_opts.DebugRenderer),
			"defaultBootEnv":      c_opts.DefaultBootEnv,
			"unknownBootEnv":      c_opts.UnknownBootEnv,
			"knownTokenTimeout":   fmt.Sprintf("%d", c_opts.KnownTokenTimeout),
			"unknownTokenTimeout": fmt.Sprintf("%d", c_opts.UnknownTokenTimeout),
		},
		publishers)

	pc, err := midlayer.InitPluginController(c_opts.PluginRoot, dt, logger, publishers)
	if err != nil {
		logger.Fatalf("Error starting plugin service: %v", err)
	} else {
		services = append(services, pc)
	}

	fe := frontend.NewFrontend(dt, logger,
		c_opts.OurAddress, c_opts.ApiPort, c_opts.FileRoot,
		c_opts.DevUI, nil, publishers, pc)

	if _, err := os.Stat(c_opts.TlsCertFile); os.IsNotExist(err) {
		buildKeys(c_opts.TlsCertFile, c_opts.TlsKeyFile)
	}
	if !c_opts.DisableProvisioner {
		logger.Printf("Starting TFTP server")
		if svc, err := midlayer.ServeTftp(fmt.Sprintf(":%d", c_opts.TftpPort), dt.FS.TftpResponder(), logger, publishers); err != nil {
			logger.Fatalf("Error starting TFTP server: %v", err)
		} else {
			services = append(services, svc)
		}

		logger.Printf("Starting static file server")
		if svc, err := midlayer.ServeStatic(fmt.Sprintf(":%d", c_opts.StaticPort), dt.FS, logger, publishers); err != nil {
			logger.Fatalf("Error starting static file server: %v", err)
		} else {
			services = append(services, svc)
		}
	}

	if !c_opts.DisableDHCP {
		logger.Printf("Starting DHCP server")
		if svc, err := midlayer.StartDhcpHandler(dt, c_opts.DhcpInterfaces, c_opts.DhcpPort, publishers); err != nil {
			logger.Fatalf("Error starting DHCP server: %v", err)
		} else {
			services = append(services, svc)
		}
	}

	srv := &http.Server{Addr: fmt.Sprintf(":%d", c_opts.ApiPort), Handler: fe.MgmtApi}
	services = append(services, srv)

	go func() {
		// Handle SIGINT and SIGTERM.
		ch := make(chan os.Signal)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		log.Println(<-ch)

		// Stop the service gracefully.
		for _, svc := range services {
			logger.Println("Shutting down server...")
			if err := svc.Shutdown(context.Background()); err != nil {
				logger.Printf("could not shutdown: %v", err)
			}
		}
	}()

	logger.Printf("Starting API server")
	if err = srv.ListenAndServeTLS(c_opts.TlsCertFile, c_opts.TlsKeyFile); err != http.ErrServerClosed {
		// Stop the service gracefully.
		for _, svc := range services {
			logger.Println("Shutting down server...")
			if err := svc.Shutdown(context.Background()); err != http.ErrServerClosed {
				logger.Printf("could not shutdown: %v", err)
			}
		}
		logger.Fatalf("Error running API service: %v\n", err)
	}
}
