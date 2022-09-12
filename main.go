// Copyright 2018 Hajime Hoshi
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

const mainWasm = "main.wasm"

const indexHTML = `<!DOCTYPE html>
<script src="wasm_exec.js"></script>
<script>
(async () => {
  const resp = await fetch({{.MainWasm}});
  if (!resp.ok) {
    const pre = document.createElement('pre');
    pre.innerText = await resp.text();
    document.body.appendChild(pre);
  } else {
    const src = await resp.arrayBuffer();
    const go = new Go();
    const result = await WebAssembly.instantiate(src, go.importObject);
    go.argv = {{.Argv}};
    go.env = {{.Env}};
    go.run(result.instance);
  }
  const reload = await fetch('_wait');
  // The server sends a response for '_wait' when a request is sent to '_notify'.
  if (reload.ok) {
    location.reload();
  }
})();
</script>
`

var (
	flagHTTP        = flag.String("http", ":8080", "HTTP bind address to serve")
	flagTags        = flag.String("tags", "", "Build tags")
	flagAllowOrigin = flag.String("allow-origin", "", "Allow specified origin (or * for all origins) to make requests to this server")
	flagOverlay     = flag.String("overlay", "", "Overwrite source files with a JSON file (see https://pkg.go.dev/cmd/go for more details)")
)

var (
	tmpOutputDir = ""
	waitChannel  = make(chan struct{})
)

func ensureTmpOutputDir() (string, error) {
	if tmpOutputDir != "" {
		return tmpOutputDir, nil
	}

	tmp, err := os.MkdirTemp("", "")
	if err != nil {
		return "", err
	}
	tmpOutputDir = tmp
	return tmpOutputDir, nil
}

func handle(w http.ResponseWriter, r *http.Request) {
	if *flagAllowOrigin != "" {
		w.Header().Set("Access-Control-Allow-Origin", *flagAllowOrigin)
	}

	output, err := ensureTmpOutputDir()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	upath := r.URL.Path[1:]
	fpath := path.Base(upath)

	if !strings.HasSuffix(r.URL.Path, "/") {
		fi, err := os.Stat(fpath)
		if err != nil && !os.IsNotExist(err) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if fi != nil && fi.IsDir() {
			http.Redirect(w, r, r.URL.Path+"/", http.StatusSeeOther)
			return
		}
	}

	switch filepath.Base(fpath) {
	case ".":
		fpath = filepath.Join(fpath, "index.html")
		fallthrough
	case "index.html":
		if _, err := os.Stat(fpath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if errors.Is(err, fs.ErrNotExist) {
			firstArg := filepath.Join(output, mainWasm)
			fargs := make([]string, flag.NArg())
			copy(fargs, flag.Args())
			if len(fargs) == 0 {
				fargs = append(fargs, firstArg)
			} else {
				fargs[0] = firstArg
			}
			argv := make([]string, 0, len(fargs))
			for _, a := range fargs {
				argv = append(argv, `"`+template.JSEscapeString(a)+`"`)
			}
			h := strings.ReplaceAll(indexHTML, "{{.Argv}}", "["+strings.Join(argv, ", ")+"]")

			oenv := os.Environ()
			env := make([]string, 0, len(oenv))
			for _, e := range oenv {
				split := strings.SplitN(e, "=", 2)
				env = append(env, `"`+template.JSEscapeString(split[0])+`": "`+template.JSEscapeString(split[1])+`"`)
			}
			h = strings.ReplaceAll(h, "{{.Env}}", "{"+strings.Join(env, ", ")+"}")

			h = strings.ReplaceAll(h, "{{.MainWasm}}", `"`+template.JSEscapeString(mainWasm)+`"`)

			http.ServeContent(w, r, "index.html", time.Now(), bytes.NewReader([]byte(h)))
			return
		}
	case "wasm_exec.js":
		if _, err := os.Stat(fpath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if errors.Is(err, fs.ErrNotExist) {
			out, err := exec.Command("go", "env", "GOROOT").Output()
			if err != nil {
				log.Print(err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			f := filepath.Join(strings.TrimSpace(string(out)), "misc", "wasm", "wasm_exec.js")
			http.ServeFile(w, r, f)
			return
		}
	case mainWasm:
		if _, err := os.Stat(fpath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if errors.Is(err, fs.ErrNotExist) {
			if err := goBuild(filepath.Join(output, mainWasm)); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}

			f, err := os.Open(filepath.Join(output, mainWasm))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer f.Close()

			http.ServeContent(w, r, mainWasm, time.Now(), f)
			return
		}
	case "_wait":
		waitForUpdate(w, r)
		return
	case "_notify":
		notifyWaiters(w, r)
		return
	}

	http.ServeFile(w, r, filepath.Join(".", r.URL.Path))
}

func goBuild(outputPath string) error {
	target := "."
	if flag.NArg() > 0 {
		target = flag.Args()[0]
	}

	absOutputPath, err := filepath.Abs(outputPath)
	if err != nil {
		return err
	}

	// buildDir is the directory to build the Wasm binary.
	buildDir := "."

	// If the target path is absolute, an environment with go.mod is required.
	if !strings.HasPrefix(target, "./") && !strings.HasPrefix(target, ".\\") {
		dir, err := os.MkdirTemp("", "")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		buildDir = dir

		// Run `go mod init`.
		cmd := exec.Command("go", "mod", "init", "foo")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if len(out) > 0 {
			log.Print(string(out))
		}
		if err != nil {
			return err
		}

		// Run `go get`.
		cmd = exec.Command("go", "get", target)
		cmd.Dir = dir
		out, err = cmd.CombinedOutput()
		if len(out) > 0 {
			log.Print(string(out))
		}
		if err != nil {
			return err
		}

		// `go build` cannot accept a path with a version. Drop it.
		if idx := strings.LastIndex(target, "@"); idx >= 0 {
			target = target[:idx]
		}
	}

	// Run `go build`.
	args := []string{"build"}
	if *flagTags != "" {
		args = append(args, "-tags", *flagTags)
	}
	if *flagOverlay != "" {
		args = append(args, "-overlay", *flagOverlay)
	}
	args = append(args, "-o", absOutputPath)
	args = append(args, target)
	log.Print("go ", strings.Join(args, " "))

	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	cmd.Dir = buildDir

	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		log.Print(string(out))
	}
	if err != nil {
		return err
	}

	return nil
}

func waitForUpdate(w http.ResponseWriter, r *http.Request) {
	waitChannel <- struct{}{}
	http.ServeContent(w, r, "", time.Now(), bytes.NewReader(nil))
}

func notifyWaiters(w http.ResponseWriter, r *http.Request) {
	for {
		select {
		case <-waitChannel:
		default:
			http.ServeContent(w, r, "", time.Now(), bytes.NewReader(nil))
			return
		}
	}
}

func main() {
	flag.Parse()
	http.HandleFunc("/", handle)
	log.Fatal(http.ListenAndServe(*flagHTTP, nil))
}
