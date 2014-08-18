package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/bioothod/backrunner/errors"
	"github.com/bioothod/backrunner/bucket"
	"github.com/bioothod/backrunner/etransport"
	"log"
	"math/rand"
	"net/http"
	"runtime"
	"strings"
	"time"
)

var (
	proxy bproxy

	nobucket_upload_prefix	= "nobucket_upload"
	bucket_upload_prefix	= "upload"
	get_prefix		= "get"
	lookup_prefix		= "lookup"
	ping_prefix		= "ping"
	IdleTimeout		= 5 * time.Second
)

type bproxy struct {
	host		string
	bctl		*bucket.BucketCtl
	ell		*etransport.Elliptics
}

func Key(req *http.Request, prefix string) string {
	return req.URL.Path[len(prefix):]
}

func (p *bproxy) local_url(key, bucket, operation string) string {
	return fmt.Sprintf("http://%s/%s/%s/%s", p.host, operation, bucket, key)
}

func (p *bproxy) send_upload_reply(w http.ResponseWriter, req *http.Request, bucket *bucket.Bucket, key string, resp map[string]interface{}) {
	type ent_reply struct {
		Get    string `json:"get"`
		Update string `json:"update"`
		Delete string `json:"delete"`
		Key    string `json:"key"`
	}
	type upload_reply struct {
		Bucket  string       `json:"bucket"`
		Primary ent_reply    `json:"primary"`
		Reply   *map[string]interface{} `json:"reply"`
	}

	reply := upload_reply {
		Bucket: bucket.Name,
		Reply:  &resp,
		Primary: ent_reply{
			Key:	key,
			Get:    "GET " + proxy.local_url(key, bucket.Name, "get"),
			Update: "POST " + proxy.local_url(key, bucket.Name, "upload"),
			Delete: "POST " + proxy.local_url(key, bucket.Name, "delete"),
		},
	}

	reply_json, err := json.Marshal(reply)
	if err != nil {
		log.Printf("url: %s: upload: json marshal failed: %q\n", req.URL, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(reply_json)
}

func upload_handler(w http.ResponseWriter, req *http.Request, key string) {
	resp, bucket, err := proxy.bctl.Upload(key, req)
	if err != nil {
		http.Error(w, errors.ErrorData(err), errors.ErrorStatus(err))
		return
	}

	proxy.send_upload_reply(w, req, bucket, key, resp)
	return
}

func ping_handler(w http.ResponseWriter, r *http.Request) {
	message := "Ping OK"

	buckets := make([]interface{}, 0)

	for i := range proxy.bctl.Bucket {
		b := &proxy.bctl.Bucket[i]
		buckets = append(buckets, b)
	}

	j, err := json.Marshal(buckets)
	if err == nil {
		message = string(j)
	} else {
		message = fmt.Sprintf("marshaling error: %v", err)
	}
	http.Error(w, message, http.StatusOK)
}

func bucket_upload_handler(w http.ResponseWriter, req *http.Request, bucket, key string) {
	resp, b, err := proxy.bctl.BucketUpload(bucket, key, req)
	if err != nil {
		http.Error(w, errors.ErrorData(err), errors.ErrorStatus(err))
		return
	}

	proxy.send_upload_reply(w, req, b, key, resp)
	return
}

func get_handler(w http.ResponseWriter, req *http.Request, bucket, key string) {
	resp, err := proxy.bctl.Get(bucket, key, req)
	if err != nil {
		http.Error(w, errors.ErrorData(err), errors.ErrorStatus(err))
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(resp)
}

func lookup_handler(w http.ResponseWriter, req *http.Request, bucket, key string) {
	reply, err := proxy.bctl.Lookup(bucket, key, req)
	if err != nil {
		http.Error(w, errors.ErrorData(err), errors.ErrorStatus(err))
		return
	}

	reply_json, err := json.Marshal(reply)
	if err != nil {
		log.Printf("url: %s: lookup: json marshal failed: %q\n", req.URL, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write(reply_json)
}

func generic_handler(w http.ResponseWriter, req *http.Request) {
	kbstrings := strings.SplitN(req.URL.Path, "/", 3)
	if len(kbstrings) < 1 {
		err := errors.NewKeyError(req.URL.String(), http.StatusBadRequest, "could not split path to /handler, there must be at least 1 slash")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	handler := kbstrings[1]

	switch handler {
	case ping_prefix:
		ping_handler(w, req)
		return
	}

	if len(kbstrings) < 2 {
		err := errors.NewKeyError(req.URL.String(), http.StatusBadRequest, "could not split path to /handler/key, there must be at least 2 slashes")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	key := kbstrings[2]

	switch handler {
	case nobucket_upload_prefix:
		upload_handler(w, req, key)
	}

	kbstrings = strings.SplitN(req.URL.Path, "/", 4)
	if len(kbstrings) < 3 {
		err := errors.NewKeyError(req.URL.String(), http.StatusBadRequest, "could not split path to /handler/bucket/key, there must be at least 3 slashes")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	bucket := kbstrings[2]
	key = kbstrings[3]

	switch handler {
	case bucket_upload_prefix:
		bucket_upload_handler(w, req, bucket, key)
	case get_prefix:
		get_handler(w, req, bucket, key)
	case lookup_prefix:
		lookup_handler(w, req, bucket, key)
	default:
		err := errors.NewKeyError(req.URL.String(), http.StatusBadRequest, "there is no registered handler for this path")
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func getTimeoutServer(addr string, handler http.Handler) *http.Server {
	//keeps people who are slow or are sending keep-alives from eating all our sockets
	return &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  IdleTimeout,
		WriteTimeout: IdleTimeout,
	}
}

type stringslice []string

func (str *stringslice) String() string {
	return fmt.Sprintf("%d", *str)
}

func (str *stringslice) Set(value string) error {
	*str = append(*str, value)
	return nil
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	listen := flag.String("listen", "0.0.0.0:9090", "listen and serve address")
	buckets := flag.String("buckets", "", "buckets file (file format: new-line separated list of bucket names)")
	config := flag.String("config", "", "Transport config file")
	flag.Parse()

	if *buckets == "" {
		log.Fatal("there is no buckets file")
	}

	if *config == "" {
		log.Fatal("You must specify config file")
	}
	var err error
	proxy.ell, err = etransport.NewEllipticsTransport(*config)

	if err != nil {
		log.Fatalf("Could not create Elliptics transport: %v", err)
	}

	rand.Seed(9)

	proxy.bctl, err = bucket.NewBucketCtl(*buckets, proxy.ell)
	if err != nil {
		log.Fatal("Could not process buckets file '"+*buckets+"'", err)
	}

	proxy.host = "localhost"


	server := getTimeoutServer(*listen, http.HandlerFunc(generic_handler))

	log.Fatal(server.ListenAndServe())
}
