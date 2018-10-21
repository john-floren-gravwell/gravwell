/*************************************************************************
 * Copyright 2017 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/gravwell/ingest"
	"github.com/gravwell/ingest/entry"
	"github.com/gravwell/ingesters/args"
	"github.com/gravwell/ingesters/version"
	"github.com/gravwell/timegrinder"
)

var (
	tso     = flag.String("timestamp-override", "", "Timestamp override")
	inFile  = flag.String("i", "", "Input file to process")
	ver     = flag.Bool("v", false, "Print version and exit")
	utc     = flag.Bool("utc", false, "Assume UTC time")
	verbose = flag.Bool("verbose", false, "Print every step")

	nlBytes = []byte("\n")
)

func init() {
	flag.Parse()
	if *ver {
		version.PrintVersion(os.Stdout)
		ingest.PrintVersion(os.Stdout)
		os.Exit(0)
	}
}

func main() {
	if *inFile == "" {
		log.Fatal("Input file path required")
	}
	timestampOverride := -1
	a, err := args.Parse()
	if err != nil {
		log.Fatalf("Invalid arguments: %v\n", err)
	}
	if len(a.Tags) != 1 {
		log.Fatal("File oneshot only accepts a single tag")
	}

	//resolve the timestmap override if there is one
	if *tso != "" {
		if timestampOverride, err = timegrinder.FormatDirective(*tso); err != nil {
			log.Fatalf("Invalid timestamp override: %v\n", err)
		}
	}

	//get a handle on the input file
	fin, err := os.Open(*inFile)
	if err != nil {
		log.Fatalf("Failed to open %s: %v\n", *inFile, err)
	}

	//fire up a uniform muxer
	igst, err := ingest.NewUniformIngestMuxer(a.Conns, a.Tags, a.IngestSecret, a.TLSPublicKey, a.TLSPrivateKey, "")
	if err != nil {
		log.Fatalf("Failed to create new ingest muxer: %v\n", err)
	}
	if err := igst.Start(); err != nil {
		log.Fatalf("Failed to start ingest muxer: %v\n", err)
	}
	if err := igst.WaitForHot(a.Timeout); err != nil {
		log.Fatalf("Failed to wait for hot connection: %v\n", err)
	}
	tag, err := igst.GetTag(a.Tags[0])
	if err != nil {
		log.Fatalf("Failed to resolve tag %s: %v\n", a.Tags[0], err)
	}

	//go ingest the file
	if err := ingestFile(fin, igst, tag, timestampOverride); err != nil {
		log.Fatalf("Failed to ingest file: %v\n", err)
	}

	if err = igst.Sync(a.Timeout); err != nil {
		log.Fatalf("Failed to sync ingest muxer: %v\n", err)
	}
	if err := igst.Close(); err != nil {
		log.Fatalf("Failed to close the ingest muxer: %v\n", err)
	}
	if err := fin.Close(); err != nil {
		log.Fatalf("Failed to close the input file: %v\n", err)
	}
}

func ingestFile(fin *os.File, igst *ingest.IngestMuxer, tag entry.EntryTag, tso int) error {
	var bts []byte
	var ts time.Time
	var ok bool
	//build a new timegrinder
	c := timegrinder.Config{
		EnableLeftMostSeed: true,
	}
	if tso > 0 {
		c.FormatOverride = tso
	}
	tg, err := timegrinder.NewTimeGrinder(c)
	if err != nil {
		return err
	}
	if *utc {
		tg.SetUTC()
	}

	src, err := igst.SourceIP()
	if err != nil {
		return err
	}

	scn := bufio.NewScanner(fin)
	for scn.Scan() {
		if bts = bytes.TrimSuffix(scn.Bytes(), nlBytes); len(bts) == 0 {
			continue
		}
		if ts, ok, err = tg.Extract(bts); err != nil {
			return err
		} else if !ok {
			ts = time.Now()
		}
		ent := &entry.Entry{
			TS:  entry.FromStandard(ts),
			Tag: tag,
			SRC: src,
		}
		ent.Data = append(ent.Data, bts...) //force reallocation due to the scanner
		if err = igst.WriteEntry(ent); err != nil {
			return err
		}
		if *verbose {
			fmt.Println(ent.TS, ent.Tag, ent.SRC, string(ent.Data))
		}
	}

	return nil
}
