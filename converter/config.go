// Copyright 2013 The Agostle Authors. All rights reserved.
// Use of this source code is governed by an Apache 2.0
// license that can be found in the LICENSE file.

// Package converter implements function for converting files to PDF
package converter

import (
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"golang.org/x/net/context"

	"github.com/go-kit/kit/log"
	"github.com/stvp/go-toml-config"
	"github.com/tgulacsi/go/osgroup"
)

var Logger *log.Context

func lookPath(fn string) string {
	path, err := exec.LookPath(fn)
	if err != nil {
		return ""
	}
	return path
}

var (
	// ConfPdftk is the path for PdfTk
	ConfPdftk = config.String("pdftk", lookPath("pdftk"))

	// ConfPdfseparate is the path for pdfseparate (member of poppler-utils
	ConfPdfseparate = config.String("pdfseparate", "pdfseparate")

	// ConfLoffice is the path for LibreOffice
	ConfLoffice = config.String("loffice", lookPath("loffice"))

	// ConfGm is the path for GraphicsMagick
	ConfGm = config.String("gm", lookPath("gm"))

	// ConfGs is the path for GhostScript
	ConfGs = config.String("gs", lookPath("gs"))

	// ConfPdfClean is the path for pdfclean
	ConfPdfClean = config.String("pdfclean", lookPath("pdfclean"))

	// ConfMutool is the path for mutool
	ConfMutool = config.String("mutool", lookPath("mutool"))

	// ConvWkhtmltopdf is the parth for wkhtmltopdf
	ConfWkhtmltopdf = config.String("wkhtmltopdf", lookPath("wkhtmltopdf"))

	// ConfSortBeforeMerge should be true if generally we should sort files by filename before merge
	ConfSortBeforeMerge = config.Bool("sortBeforeMerge", false)

	// ConfChildTimeout is the time before the child gets killed
	ConfChildTimeout = config.Duration("childTimeout", 1*time.Hour)

	// ConcLimit limits the concurrently running child processes
	ConcLimit = NewRateLimiter(Concurrency)

	// ConfWorkdir is the working directory (will be os.TempDir() if empty)
	ConfWorkdir = config.String("workdir", "")

	// ConfListenAddr is a listen address for HTTP requests
	ConfListenAddr = config.String("listen", ":9500")

	// ConfDefaultIsService decides whether start as service without args
	ConfDefaultIsService = config.Bool("defaultIsService", false)

	// ConfUseLofficePortLock defines whether to limit Loffice usage by a port lock
	ConfLofficeUsePortLock = config.Bool("lofficeUsePortLock", !osgroup.IsInsideDocker())

	// ConfLogFile specifies the file to log - instead of command line.
	ConfLogFile = config.String("logfile", "")
)

// LoadConfig loads TOML config file
func LoadConfig(fn string) error {
	if err := config.Parse(fn); err != nil {
		Log("msg", "WARN Cannot open config file", "file", fn, "error", err)
	}
	if *ConfLoffice != "" {
		if _, err := exec.LookPath(*ConfLoffice); err != nil {
			Log("msg", "WARN cannot use as loffice!", "loffice", *ConfLoffice)
			if fn, err := exec.LookPath("soffice"); err == nil {
				Log("msg", "Will use as loffice instead.", "soffice", fn)
				*ConfLoffice = fn
			}
		}
	}
	if *ConfWorkdir != "" {
		_ = os.Setenv("TMPDIR", *ConfWorkdir)
		Workdir = *ConfWorkdir
	}

	bn := filepath.Base(*ConfPdfseparate)
	prefix := (*ConfPdfseparate)[:len(*ConfPdfseparate)-len(bn)]
	for k := range popplerOk {
		if err := exec.Command(prefix+k, "-h").Run(); err == nil {
			popplerOk[k] = prefix + k
		}
	}
	Log("popplerOk", popplerOk)

	if !*ConfLofficeUsePortLock {
		lofficeMu.Lock()
		lofficePortLock = nil
		lofficeMu.Unlock()
	}

	return nil
}

// Workdir is the main working directory
var Workdir = os.TempDir()

// LeaveTempFiles should be true only for debugging purposes (leaves temp files)
var LeaveTempFiles = false

func prepareContext(ctx context.Context, subdir string) (context.Context, string) {
	const wdKey = "workdir"
	odir, _ := ctx.Value(wdKey).(string)
	if odir != "" {
		if subdir != "" {
			ctx = context.WithValue(ctx, wdKey, filepath.Join(Workdir, subdir))
		}
	} else {
		if subdir != "" {
			ctx = context.WithValue(ctx, wdKey, Workdir)
		} else {
			ctx = context.WithValue(ctx, wdKey, filepath.Join(Workdir, subdir))
		}
	}
	ndir, ok := ctx.Value(wdKey).(string)
	if ok && odir != ndir {
		if err := os.MkdirAll(ndir, 0750); err != nil {
			panic("cannot create workdir " + ndir + ": " + err.Error())
		}
	}
	return ctx, ndir
}

// port for LibreOffice locking (only one instance should be running)
const LofficeLockPort = 27999

// save original html (do not delete it)
var SaveOriginalHTML = false

// name of errors list in resulting archive
const ErrTextFn = "ZZZ-errors.txt"

func getLogger(ctx context.Context) *log.Context {
	if ctx == nil {
		return Logger
	}
	if logger, ok := ctx.Value("logger").(*log.Context); ok {
		return logger
	}
	return Logger
}

func Log(keyvals ...interface{}) {
	if Logger == nil {
		return
	}
	Logger.Log(keyvals...)
}
