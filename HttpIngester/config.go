/*************************************************************************
 * Copyright 2018 Gravwell, Inc. All rights reserved.
 * Contact: <legal@gravwell.io>
 *
 * This software may be modified and distributed under the terms of the
 * BSD 2-clause license. See the LICENSE file for details.
 **************************************************************************/

package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/gravwell/ingest"
	"github.com/gravwell/ingest/config"

	"gopkg.in/gcfg.v1"
)

const (
	maxConfigSize  int64 = (1024 * 1024 * 2) //2MB, even this is crazy large
	defaultMaxBody int   = 4 * 1024 * 1024
	defaultLogLoc        = `/opt/gravwell/log/gravwell_http_ingester.log`

	defaultMethod string = `POST`
)

type gbl struct {
	config.IngestConfig
	Bind                 string
	Max_Body             int
	Log_Location         string
	TLS_Certificate_File string
	TLS_Key_File         string
}

type cfgReadType struct {
	Global   gbl
	Listener map[string]*lst
}

type lst struct {
	auth                             //authentication information
	URL                       string //the URL we will listen to
	Method                    string //method the listener expects
	Tag_Name                  string //the tag to assign to the request
	Ignore_Timestamps         bool   //Just apply the current timestamp to lines as we get them
	Assume_Local_Timezone     bool
	Timezone_Override         string
	Timestamp_Format_Override string //override the timestamp format
}

type cfgType struct {
	gbl
	Listener map[string]*lst
}

func GetConfig(path string) (*cfgType, error) {
	var content []byte
	fin, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := fin.Stat()
	if err != nil {
		fin.Close()
		return nil, err
	}
	//This is just a sanity check
	if fi.Size() > maxConfigSize {
		fin.Close()
		return nil, errors.New("Config File Far too large")
	}
	content = make([]byte, fi.Size())
	n, err := fin.Read(content)
	fin.Close()
	if int64(n) != fi.Size() {
		return nil, errors.New("Failed to read config file")
	}

	var cr cfgReadType
	if err := gcfg.ReadStringInto(&cr, string(content)); err != nil {
		return nil, err
	}
	c := &cfgType{
		gbl:      cr.Global,
		Listener: cr.Listener,
	}
	if err := verifyConfig(c); err != nil {
		return nil, err
	}
	return c, nil
}

func verifyConfig(c *cfgType) error {
	if err := c.IngestConfig.Verify(); err != nil {
		return err
	}
	if c.Bind == `` {
		return fmt.Errorf("No bind string specified")
	}
	if err := c.ValidateTLS(); err != nil {
		return err
	}
	urls := map[string]string{}
	if len(c.Listener) == 0 {
		return errors.New("No Sniffers specified")
	}
	for k, v := range c.Listener {
		var pth string
		if len(v.URL) == 0 {
			return errors.New("No URL provided for " + k)
		}
		p, err := url.Parse(v.URL)
		if err != nil {
			return fmt.Errorf("URL structure is invalid: %v", err)
		}
		if p.Scheme != `` {
			return errors.New("May not specify scheme in listening URL")
		} else if p.Host != `` {
			return errors.New("May not specify host in listening URL")
		}
		pth = p.Path

		if orig, ok := urls[pth]; ok {
			return fmt.Errorf("URL %s duplicated in %s (was in %s)", v.URL, k, orig)
		}
		urls[pth] = k
		//validate the auth
		if enabled, err := v.auth.Validate(); err != nil {
			return fmt.Errorf("Auth for %s is invalid: %v", k, err)
		} else if enabled && v.LoginURL != `` {
			//check the url
			if orig, ok := urls[v.LoginURL]; ok {
				return fmt.Errorf("URL %s duplicated in %s (was in %s)", v.LoginURL, k, orig)
			}
			urls[v.LoginURL] = k
		}
		if len(v.Tag_Name) == 0 {
			v.Tag_Name = `default`
		}
		if strings.ContainsAny(v.Tag_Name, ingest.FORBIDDEN_TAG_SET) {
			return errors.New("Invalid characters in the \"" + v.Tag_Name + "\"Tag-Name for " + k)
		}
		//normalize the path
		v.URL = pth
		if v.Method == `` {
			v.Method = defaultMethod
		}
		c.Listener[k] = v
	}
	if len(urls) == 0 {
		return fmt.Errorf("No listeners specified")
	}
	return nil
}

// Generate a list of all tags used by this ingester
func (c *cfgType) Tags() (tags []string, err error) {
	tagMp := make(map[string]bool, 1)
	for _, v := range c.Listener {
		if len(v.Tag_Name) == 0 {
			continue
		}
		if _, ok := tagMp[v.Tag_Name]; !ok {
			tags = append(tags, v.Tag_Name)
			tagMp[v.Tag_Name] = true
		}
	}
	if len(tags) == 0 {
		err = errors.New("No tags specified")
	} else {
		sort.Strings(tags)
	}
	return
}

func (c *cfgType) LogLoc() string {
	if c.Log_Location == `` {
		return defaultLogLoc
	}
	return c.Log_Location
}

func (c *cfgType) MaxBody() int {
	if c.Max_Body <= 0 {
		return defaultMaxBody
	}
	return c.Max_Body
}

func (g gbl) ValidateTLS() (err error) {
	if !g.TLSEnabled() {
		//not enabled
	} else if g.TLS_Certificate_File == `` {
		err = errors.New("TLS-Certificate-File argument is missing")
	} else if g.TLS_Key_File == `` {
		err = errors.New("TLS-Key-File argument is missing")
	} else {
		_, err = tls.LoadX509KeyPair(g.TLS_Certificate_File, g.TLS_Key_File)
	}
	return
}

func (g gbl) TLSEnabled() (r bool) {
	r = g.TLS_Certificate_File != `` && g.TLS_Key_File != ``
	return
}
