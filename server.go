// Copyright 2013 The Agostle Authors. All rights reserved.
// Use of this source code is governed by an Apache 2.0
// license that can be found in the LICENSE file.

package main

// Needed: /email/convert?splitted=1&errors=1&id=xxx Accept: images/gif
//  /pdf/merge Accept: application/zip

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httputil"
	_ "net/http/pprof"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/net/context"

	"github.com/oklog/ulid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tgulacsi/agostle/converter"
	"github.com/tgulacsi/go/temp"

	"github.com/go-kit/kit/log"
	kithttp "github.com/go-kit/kit/transport/http"

	"gopkg.in/tylerb/graceful.v1"
)

var (
	defaultImageSize = "640x640"
	self             = ""
	sortBeforeMerge  = false
)

// newHTTPServer returns a new, stoppable HTTP server
func newHTTPServer(address string, saveReq bool) *graceful.Server {
	onceOnStart.Do(onStart)

	if saveReq {
		defaultBeforeFuncs = append(defaultBeforeFuncs, dumpRequest)
	}

	mux := http.DefaultServeMux
	//mux.Handle("/debug/pprof", pprof.Handler)
	mux.Handle("/metrics", prometheus.Handler())

	H := func(path string, handleFunc http.HandlerFunc) {
		mux.HandleFunc(path,
			prometheus.InstrumentHandler(strings.Replace(path[1:], "/", "_", -1),
				handleFunc))
	}
	H("/pdf/merge", pdfMergeServer.ServeHTTP)
	H("/email/convert", emailConvertServer.ServeHTTP)
	H("/outlook", outlookToEmailServer.ServeHTTP)
	mux.Handle("/_admin/stop", http.HandlerFunc(adminStopHandler))
	mux.Handle("/", http.HandlerFunc(statusPage))

	s := &graceful.Server{
		Server: &http.Server{
			Addr:         address,
			ReadTimeout:  300 * time.Second,
			WriteTimeout: 1800 * time.Second,
			Handler:      mux,
		},
		Timeout: 5 * time.Minute,
	}
	return s
}

func SetRequestID(ctx context.Context, name string) context.Context {
	if name == "" {
		name = "reqid"
	}
	if ctx.Value(name) != nil {
		return ctx
	}
	return context.WithValue(ctx, name, NewULID().String())
}
func GetRequestID(ctx context.Context, name string) string {
	if v, ok := ctx.Value(name).(string); ok && v != "" {
		return v
	}
	return NewULID().String()
}

func NewULID() ulid.ULID {
	return ulid.MustNew(ulid.Now(), rand.Reader)
}

var defaultBeforeFuncs = []kithttp.RequestFunc{
	prepareContext,
}

func prepareContext(ctx context.Context, r *http.Request) context.Context {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	ctx = context.WithValue(ctx, "cancel", cancel)
	ctx = SetRequestID(ctx, "")
	lgr := getLogger(ctx)
	lgr = lgr.With(
		"reqid", GetRequestID(ctx, ""),
		"path", r.URL.Path,
		"method", r.Method,
	)
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		lgr = lgr.With("ip", host)
	}
	ctx = context.WithValue(ctx, "logger", lgr)
	logAccept(ctx, r)
	return ctx
}

func dumpRequest(ctx context.Context, req *http.Request) context.Context {
	prefix := filepath.Join(converter.Workdir, time.Now().Format("20060102_150405")+"-")
	var reqSeq uint64
	b, err := httputil.DumpRequest(req, true)
	Log := getLogger(ctx).With("fn", "dumpRequest").Log
	if err != nil {
		Log("msg", "dumping request", "error", err)
	}
	fn := fmt.Sprintf("%s%06d.dmp", prefix, atomic.AddUint64(&reqSeq, 1))
	if err = ioutil.WriteFile(fn, b, 0660); err != nil {
		Log("msg", "writing", "dumpfile", fn, "error", err)
	} else {
		Log("msg", "Request has been dumped into "+fn)
	}
	return ctx
}

// startHTTPServerListener starts the server on the address, and NEVER RETURNS!
func startHTTPServerListener(listener net.Listener, saveReq bool) {
	s := newHTTPServer("", saveReq)
	Log := logger.Log
	Log("msg", "Start listening on", "listener", listener)
	if err := s.Serve(listener); err != nil {
		Log("msg", "Serve", "error", err)
		os.Exit(1)
	}
}

func adminStopHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Refresh", "3;URL=/")
	w.WriteHeader(200)
	fmt.Fprintf(w, `Stopping...`)
	go func() {
		time.Sleep(time.Millisecond * 500)
		logger.Log("msg", "SUICIDE for ask!")
		os.Exit(3)
	}()
}

type reqFile struct {
	multipart.FileHeader
	io.ReadCloser
}

// getOneRequestFile reads the first file from the request (if multipart/),
// or returns the body if not
func getOneRequestFile(ctx context.Context, r *http.Request) (reqFile, error) {
	f := reqFile{ReadCloser: r.Body}
	contentType := r.Header.Get("Content-Type")
	getLogger(ctx).Log("msg", "readRequestOneFile", "content-type", contentType)
	if !strings.HasPrefix(contentType, "multipart/") {
		f.FileHeader.Header = textproto.MIMEHeader(r.Header)
		return f, nil
	}
	defer func() { _ = r.Body.Close() }()
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return f, errors.New("error parsing request as multipart-form: " + err.Error())
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File) == 0 {
		return f, errors.New("no files?")
	}

	for _, fileHeaders := range r.MultipartForm.File {
		for _, fileHeader := range fileHeaders {
			var err error
			if f.ReadCloser, err = fileHeader.Open(); err != nil {
				return f, fmt.Errorf("error opening part %q: %s", fileHeader.Filename, err)
			}
			f.FileHeader = *fileHeader
			return f, nil
		}
	}
	return reqFile{}, nil
}

// getRequestFiles reads the files from the request, and calls readerToFile on them
func getRequestFiles(r *http.Request) ([]reqFile, error) {
	if r.Body != nil {
		defer func() { _ = r.Body.Close() }()
	}
	err := r.ParseMultipartForm(1 << 20)
	if err != nil {
		return nil, errors.New("cannot parse request as multipart-form: " + err.Error())
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File) == 0 {
		return nil, errors.New("no files?")
	}

	files := make([]reqFile, 0, len(r.MultipartForm.File))
	for _, fileHeaders := range r.MultipartForm.File {
		for _, fileHeader := range fileHeaders {
			f := reqFile{FileHeader: *fileHeader}
			if f.ReadCloser, err = fileHeader.Open(); err != nil {
				return nil, fmt.Errorf("error reading part %q: %s", fileHeader.Filename, err)
			}
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return nil, errors.New("no files??")
	}
	return files, nil
}

// readerToFile copies the reader to a temp file and returns its name or error
func readerToFile(r io.Reader, prefix string) (filename string, err error) {
	dfh, e := ioutil.TempFile("", "agostle-"+baseName(prefix)+"-")
	if e != nil {
		err = e
		return
	}
	if sfh, ok := r.(*os.File); ok {
		filename = dfh.Name()
		_ = dfh.Close()
		_ = os.Remove(filename)
		err = temp.LinkOrCopy(sfh.Name(), filename)
		return
	}
	if _, err = io.Copy(dfh, r); err == nil {
		filename = dfh.Name()
	}
	_ = dfh.Close()
	return
}

func tempFilename(prefix string) (filename string, err error) {
	fh, e := ioutil.TempFile("", prefix)
	if e != nil {
		err = e
		return
	}
	filename = fh.Name()
	_ = fh.Close()
	return
}

func logAccept(ctx context.Context, r *http.Request) {
	getLogger(ctx).Log("msg", "ACCEPT", "method", r.Method, "uri", r.RequestURI, "remote", r.RemoteAddr)
}

func baseName(fileName string) string {
	if fileName == "" {
		return ""
	}
	i := strings.LastIndexAny(fileName, "/\\")
	if i >= 0 {
		fileName = fileName[i+1:]
	}
	return fileName
}
func getLogger(ctx context.Context) *log.Context {
	if ctx == nil {
		return logger
	}
	if logger, ok := ctx.Value("logger").(*log.Context); ok {
		return logger
	}
	return logger
}
