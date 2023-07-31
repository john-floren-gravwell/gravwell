/*************************************************************************
 * Copyright 2020 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/

package base

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"

	"github.com/gravwell/gravwell/v3/ingest"
	"github.com/gravwell/gravwell/v3/ingest/attach"
	"github.com/gravwell/gravwell/v3/ingest/config"
	"github.com/gravwell/gravwell/v3/ingest/config/validate"
	"github.com/gravwell/gravwell/v3/ingest/log"
	"github.com/gravwell/gravwell/v3/ingesters/version"
)

var (
	baseConfig IngesterBaseConfig

	ErrInvalidParameter = errors.New("invalid parameter")
	ErrNotReady         = errors.New("IngesterBase is not ready")
)

type getConfigFunc func(cfg, overlay string) (interface{}, error)

type cfgHelper interface {
	Tags() ([]string, error)
	IngestBaseConfig() config.IngestConfig
	AttachConfig() attach.AttachConfig
}

type IngesterBaseConfig struct {
	IngesterName                 string
	AppName                      string
	DefaultConfigLocation        string
	DefaultConfigOverlayLocation string
	GetConfigFunc                interface{}
}

type IngesterBase struct {
	IngesterBaseConfig
	Verbose bool
	Logger  *log.Logger
	Cfg     interface{}
}

func Init(ibc IngesterBaseConfig) (ib IngesterBase, err error) {
	ib.IngesterBaseConfig = ibc
	confLoc := flag.String("config-file", ibc.DefaultConfigLocation, "Location for configuration file")
	confdLoc := flag.String("config-overlays", ibc.DefaultConfigOverlayLocation, "Location for configuration overlay files")
	verbose := flag.Bool("v", false, "Display verbose status updates to stdout")
	stderrOverride := flag.String("stderr", "", "Redirect stderr to a shared memory file")
	ver := flag.Bool("version", false, "Print the version information and exit")

	flag.Parse()
	if *ver {
		version.PrintVersion(os.Stdout)
		ingest.PrintVersion(os.Stdout)
		os.Exit(0)
	}
	if err = ibc.validate(); err != nil {
		return
	}

	validate.ValidateConfig(ibc.GetConfigFunc, *confLoc, *confdLoc)
	var fp string
	if pth := filepath.Clean(*stderrOverride); pth != `` && pth != `.` {
		fp = filepath.Join(`/dev/shm/`, pth)
	}
	cb := func(w io.Writer) {
		version.PrintVersion(w)
		ingest.PrintVersion(w)
		log.PrintOSInfo(w)
	}
	if ib.Logger, err = log.NewStderrLoggerEx(fp, cb); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get stderr logger: %v\n", err)
		return
	}
	ib.Logger.SetAppname(ibc.AppName)
	ib.Verbose = *verbose
	debug.SetTraceback("all")

	//now try to call getConfig and extract the base ingester configuration
	var ch cfgHelper
	if ib.Cfg, ch, err = ibc.getConfig(*confLoc, *confdLoc); err != nil {
		return
	}
	cfg := ch.IngestBaseConfig()
	if cfg.Disable_Multithreading {
		//go into single threaded mode
		runtime.GOMAXPROCS(1)
	}

	cfg.AddLocalLogging(ib.Logger)
	return
}

// AssignConfig is a helper function that can take care of most of the sanity checking
// when trying to turn a native config object generated by IngesterBaseConfig.GetConfigFunc
// into the native type assigned into IngesterBase.Cfg.
//
// Basically we are going to use some reflect foo to reduct the amount of boiler plate code
// needed by users of this library
func (ib *IngesterBase) AssignConfig(v interface{}) (err error) {
	//preflight checks
	if v == nil {
		return ErrInvalidParameter
	} else if ib == nil || ib.Cfg == nil {
		return ErrNotReady
	}
	//check that the value handed in is a pointer
	if reflect.ValueOf(v).Kind() != reflect.Ptr {
		return ErrInvalidParameter
	}
	vv := reflect.ValueOf(v).Elem() //get a handle on the incoming interface pointer value
	sv := reflect.ValueOf(ib.Cfg)   // get a handle on the source interface value

	if vv.Type() != sv.Type() {
		return fmt.Errorf("Type Mismatch: %T != %T", v, ib.Cfg)
	} else if !sv.Type().AssignableTo(vv.Type()) || !vv.CanSet() {
		return fmt.Errorf("%T cannot be assigned into %T", ib.Cfg, v)
	}

	//ok... do the actual assignment, this should almost always be a pointer to a pointer
	vv.Set(sv)
	return nil
}

func (ib *IngesterBase) GetMuxer() (igst *ingest.IngestMuxer, err error) {
	//now try to call getConfig and extract the base ingester configuration
	if ib.Cfg == nil {
		err = errors.New("nil config")
		return
	}

	ch, ok := ib.Cfg.(cfgHelper)
	if !ok {
		err = fmt.Errorf("Config type %T does not implement the helper interface", ib.Cfg)
		return
	}
	var tags []string
	if tags, err = ch.Tags(); err != nil {
		err = fmt.Errorf("Failed to get tags %w", err)
		return
	}
	cfg := ch.IngestBaseConfig()

	conns, err := cfg.Targets()
	if err != nil {
		ib.Logger.FatalCode(0, "failed to get backend targets from configuration", log.KVErr(err))
		return
	}
	ib.Debug("Handling %d tags over %d targets\n", len(tags), len(conns))

	lmt, err := cfg.RateLimit()
	if err != nil {
		ib.Logger.FatalCode(0, "failed to get rate limit from configuration", log.KVErr(err))
		return
	}
	ib.Debug("Rate limiting connection to %d bps\n", lmt)

	//fire up the ingesters
	ib.Debug("INSECURE skip TLS certificate verification: %v\n", cfg.InsecureSkipTLSVerification())
	id, ok := cfg.IngesterUUID()
	if !ok {
		ib.Logger.FatalCode(0, "could not read ingester UUID")
	}
	igCfg := ingest.UniformMuxerConfig{
		IngestStreamConfig: cfg.IngestStreamConfig,
		Destinations:       conns,
		Tags:               tags,
		Auth:               cfg.Secret(),
		VerifyCert:         !cfg.InsecureSkipTLSVerification(),
		IngesterName:       ib.IngesterName,
		IngesterVersion:    version.GetVersion(),
		IngesterUUID:       id.String(),
		IngesterLabel:      cfg.Label,
		RateLimitBps:       lmt,
		Logger:             ib.Logger,
		CacheDepth:         cfg.Cache_Depth,
		CachePath:          cfg.Ingest_Cache_Path,
		CacheSize:          cfg.Max_Ingest_Cache,
		CacheMode:          cfg.Cache_Mode,
		LogSourceOverride:  net.ParseIP(cfg.Log_Source_Override),
		Attach:             ch.AttachConfig(),
	}
	if igst, err = ingest.NewUniformMuxer(igCfg); err != nil {
		ib.Logger.Fatal("failed build our ingest system", log.KVErr(err))
		return
	}

	ib.Debug("Started ingester muxer\n")
	if cfg.SelfIngest() {
		ib.Logger.AddRelay(igst)
	}
	if err := igst.Start(); err != nil {
		ib.Logger.FatalCode(0, "failed start our ingest system", log.KVErr(err))
	}
	ib.Debug("Waiting for connections to indexers ... ")
	if err := igst.WaitForHot(cfg.Timeout()); err != nil {
		ib.Logger.FatalCode(0, "timeout waiting for backend connections", log.KV("timeout", cfg.Timeout()), log.KVErr(err))
	}
	ib.Debug("Successfully connected to ingesters\n")

	// prepare the configuration we're going to send upstream
	if err = igst.SetRawConfiguration(cfg); err != nil {
		ib.Logger.FatalCode(0, "failed to set configuration for ingester state messages")
	}

	return
}

func (ib IngesterBase) Debug(format string, args ...interface{}) {
	if !ib.Verbose {
		return
	}
	fmt.Printf(format, args...)
}

func (ibc IngesterBaseConfig) validate() error {
	if ibc.IngesterName == `` {
		return errors.New("missing ingester name")
	} else if ibc.AppName == `` {
		return errors.New("missing app name")
	}

	return nil
}

func (ibc IngesterBaseConfig) getConfig(confLoc, confDLoc string) (obj interface{}, ch cfgHelper, err error) {
	if ibc.GetConfigFunc == nil {
		err = errors.New("nil get config func")
		return
	}
	// do some reflection foo to make sure what we are getting is valid
	fn := reflect.ValueOf(ibc.GetConfigFunc)
	fnType := fn.Type()
	if fnType.Kind() != reflect.Func {
		err = fmt.Errorf("Given configuration function is not a function")
		return
	} else if fnType.NumOut() != 2 {
		err = fmt.Errorf("Given configuration function produces %d output values instead of 2\n", fnType.NumOut())
		return
	}

	args := []reflect.Value{reflect.ValueOf(confLoc)}
	if argc := fnType.NumIn(); argc < 1 || argc > 2 {
		err = fmt.Errorf("Given configuration function expects %d parameters instead of 1 or 2\n", argc)
		return
	} else if argc == 2 {
		args = append(args, reflect.ValueOf(confDLoc))
	}
	res := fn.Call(args)
	if len(res) != 2 {
		err = fmt.Errorf("Given configuration function returned the wrong number of values: %d != 2\n", len(res))
		return
	}
	var ok bool
	if x := res[1].Interface(); x != nil {
		if err, ok = res[1].Interface().(error); !ok {
			err = fmt.Errorf("Given configuration function did not return an error type in second value, got %T\n", res[1].Interface())
			return
		}
	}
	obj = res[0].Interface()
	if err != nil {
		err = fmt.Errorf("Config file %q returned error %v\n", confLoc, err)
	} else if obj == nil {
		err = fmt.Errorf("Config file %q returned a nil object\n", confLoc)
	} else if ch, ok = obj.(cfgHelper); !ok {
		obj = nil
		err = fmt.Errorf("Config type %T does not implement the helper interface", obj)
	}
	return
}
