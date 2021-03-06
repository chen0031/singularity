// Copyright (c) 2018, Sylabs Inc. All rights reserved.
// This software is licensed under a 3-clause BSD license. Please consult the
// LICENSE.md file distributed with the sources of this project regarding your
// rights to use or distribute this software.

package sources

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	sytypes "github.com/singularityware/singularity/src/pkg/build/types"
	"github.com/singularityware/singularity/src/pkg/sylog"
	"github.com/singularityware/singularity/src/pkg/util/user-agent"
)

const defaultRegistry string = `singularity-hub.org/api/container/`

// ShubURI stores the various components of a singularityhub URI
type ShubURI struct {
	registry   string
	user       string
	container  string
	tag        string
	digest     string
	defaultReg bool
}

type shubAPIResponse struct {
	Image   string `json:"image"`
	Name    string `json:"name"`
	Tag     string `json:"tag"`
	Version string `json:"version"`
}

// ShubConveyorPacker only needs to hold the conveyor to have the needed data to pack
type ShubConveyorPacker struct {
	recipe   sytypes.Definition
	srcURI   ShubURI
	tmpfile  string
	manifest *shubAPIResponse
	b        *sytypes.Bundle
	localPacker
}

// Get downloads container from Singularityhub
func (cp *ShubConveyorPacker) Get(recipe sytypes.Definition) (err error) {
	sylog.Debugf("Getting container from Shub")

	cp.recipe = recipe

	src := `//` + recipe.Header["from"]

	//use custom parser to make sure we have a valid shub URI
	cp.srcURI, err = ShubParseReference(src)
	if err != nil {
		sylog.Fatalf("Invalid shub URI: %v", err)
		return
	}

	//create bundle to build into
	cp.b, err = sytypes.NewBundle("sbuild-shub")
	if err != nil {
		return
	}

	// Get the image manifest
	if err = cp.getManifest(); err != nil {
		sylog.Fatalf("Failed to get manifest from Shub: %v", err)
		return
	}

	// retrieve the image
	if err = cp.fetchImage(); err != nil {
		sylog.Fatalf("Failed to get image from Shub: %v", err)
		return
	}

	cp.localPacker, err = getLocalPacker(cp.tmpfile, cp.b)

	return err
}

// Download an image from Singularity Hub, writing as we download instead
// of storing in memory
func (cp *ShubConveyorPacker) fetchImage() (err error) {

	// Create temporary download name
	tmpfile, err := ioutil.TempFile(cp.b.Path, "shub-container")
	sylog.Debugf("\nCreating temporary image file %v\n", tmpfile.Name())
	if err != nil {
		return err
	}
	defer tmpfile.Close()

	// Get the image based on the manifest
	resp, err := http.Get(cp.manifest.Image)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Write the body to file
	bytesWritten, err := io.Copy(tmpfile, resp.Body)
	if err != nil {
		return err
	}
	//Simple check to make sure image received is the correct size
	if bytesWritten != resp.ContentLength {
		return fmt.Errorf("Image received is not the right size. Supposed to be: %v  Actually: %v", resp.ContentLength, bytesWritten)
	}

	cp.tmpfile = tmpfile.Name()
	return nil
}

// getManifest will return the image manifest for a container uri
// from Singularity Hub. We return the shubAPIResponse and error
func (cp *ShubConveyorPacker) getManifest() (err error) {

	// Create a new Singularity Hub client
	sc := http.Client{
		Timeout: 30 * time.Second,
	}

	//if we are using a non default registry error out for now
	if !cp.srcURI.defaultReg {
		return err
	}

	// Format the http address, coinciding with the image uri
	httpAddr := fmt.Sprintf("www.%s", cp.srcURI.String())

	// Create the request, add headers context
	url := url.URL{
		Scheme: "https",
		Host:   strings.Split(httpAddr, `/`)[0],     //split url to match format, first half
		Path:   strings.SplitN(httpAddr, `/`, 2)[1], //second half
	}

	req, err := http.NewRequest(http.MethodGet, url.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", useragent.Value)

	// Do the request, if status isn't success, return error
	res, err := sc.Do(req)
	sylog.Debugf("response: %v\n", res)

	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		err = errors.New(res.Status)
		return err
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}

	err = json.Unmarshal(body, &cp.manifest)
	sylog.Debugf("manifest: %v\n", cp.manifest.Image)
	if err != nil {
		return err
	}

	return nil
}

// ShubParseReference accepts a URI string and parses its content
// It will return an error if the given URI is not valid,
// otherwise it will parse the contents into a ShubURI struct
func ShubParseReference(src string) (uri ShubURI, err error) {

	//define regex for each URI component
	registryRegexp := `([-a-zA-Z0-9/]{1,64}\/)?` //target is very open, outside registry
	nameRegexp := `([-a-zA-Z0-9]{1,39}\/)`       //target valid github usernames
	containerRegexp := `([-_.a-zA-Z0-9]{1,64})`  //target valid github repo names
	tagRegexp := `(:[-_.a-zA-Z0-9]{1,64})?`      //target is very open, file extensions or branch names
	digestRegexp := `(\@[a-f0-9]{32})?`          //target md5 sum hash

	//expression is anchored
	shubRegex, err := regexp.Compile(`^\/\/` + registryRegexp + nameRegexp + containerRegexp + tagRegexp + digestRegexp + `$`)
	if err != nil {
		return uri, err
	}

	found := shubRegex.FindString(src)

	//sanity check
	//if found string is not equal to the input, input isn't a valid URI
	if strings.Compare(src, found) != 0 {
		return uri, fmt.Errorf("Source string is not a valid URI: %s", src)
	}

	//strip `//` from start of src
	src = src[2:]

	pieces := strings.SplitAfterN(src, `/`, -1)
	if l := len(pieces); l > 2 {
		//more than two pieces indicates a custom registry
		uri.defaultReg = false
		uri.registry = strings.Join(pieces[:l-2], "")
		uri.user = pieces[l-2]
		src = pieces[l-1]
	} else if l == 2 {
		//two pieces means default registry
		uri.defaultReg = true
		uri.registry = defaultRegistry
		uri.user = pieces[l-2]
		src = pieces[l-1]
	}

	//look for an @ and split if it exists
	if strings.Contains(src, `@`) {
		pieces = strings.Split(src, `@`)
		uri.digest = `@` + pieces[1]
		src = pieces[0]
	}

	//look for a : and split if it exists
	if strings.Contains(src, `:`) {
		pieces = strings.Split(src, `:`)
		uri.tag = `:` + pieces[1]
		src = pieces[0]
	}

	//container name is left over after other parts are split from it
	uri.container = src

	return uri, nil
}

func (s *ShubURI) String() string {
	return s.registry + s.user + s.container + s.tag + s.digest
}

// CleanUp removes any tmpfs owned by the conveyorPacker on the filesystem
func (cp *ShubConveyorPacker) CleanUp() {
	os.RemoveAll(cp.b.Path)
}
