package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/karalabe/iris-go"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
)

//////////////////////////////
// GitHub //
////////////

type Repository struct {
	Name string
}

type GitHubCommitReq struct {
	Repository Repository
}

func logMsg(msg string) []byte {
	log.Println(msg)
	return []byte(msg)
}

type GithubHandler struct {
	jobConf string
}

func (h *GithubHandler) HandleBroadcast(msg []byte) {
	panic("Broadcast passed to request handler")
}

func (h *GithubHandler) HandleTunnel(tun iris.Tunnel) {
	panic("Inbound tunnel on request handler")
}

func (h *GithubHandler) HandleDrop(reason error) {
	panic("Connection dropped on request handler")
}

func (h *GithubHandler) HandleRequest(req []byte) []byte {
	s := string(req)
	vals, err := url.ParseQuery(s)
	if err != nil {
		return logMsg(fmt.Sprint("ERROR Unable to parse req:", s, "-", err))
	}

	p := vals.Get("payload")
	if p == "" {
		return logMsg(fmt.Sprint("ERROR request missing 'payload' param:", s))
	}

	ghreq := GitHubCommitReq{}
	err = json.Unmarshal([]byte(p), &ghreq)
	if err != nil {
		return logMsg(fmt.Sprint("ERROR Unable to decode payload json:", p, "-", err))
	}

	return runJob(h.jobConf, ghreq)
}

//////////////////////////////
// Jobs //
//////////

type JobDef struct {
	Dir    string
	Script []string
}

func runJob(jobConf string, req GitHubCommitReq) []byte {

	repoName := req.Repository.Name

	data, err := ioutil.ReadFile(jobConf)
	if err != nil {
		return logMsg(fmt.Sprint("ERROR Unable to read jobConf file:", jobConf, "-", err))
	}

	jobsByRepo := make(map[string]JobDef)
	err = json.Unmarshal(data, &jobsByRepo)
	if err != nil {
		return logMsg(fmt.Sprint("ERROR Unable to decode JSON:", string(data), "-", err))
	}

	job, ok := jobsByRepo[repoName]
	if !ok {
		return logMsg(fmt.Sprint("ERROR No JobDef for repository:", repoName))
	}

	var cmd *exec.Cmd
	var path string

	if len(job.Script) > 0 {
		path = strings.Replace(job.Script[0], "$dir", job.Dir, 1)
	}

	switch len(job.Script) {
	case 0:
		return logMsg(fmt.Sprint("ERROR script not defined for repository: ", repoName, "-", job))
	case 1:
		cmd = exec.Command(path)
	default:
		cmd = exec.Command(path, job.Script[1:]...)
	}

	if job.Dir != "" {
		cmd.Dir = job.Dir
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	log.Println("Running job for repository:", repoName, "dir:", cmd.Dir, "script:", cmd.Path)
	err = cmd.Run()

	var msg string
	if err != nil {
		msg = fmt.Sprint("ERROR Executing script for repository:", repoName, "-", cmd.Path,
			"-", err, " stdout: ", stdout.String(), " stderr: ", stderr.String())
	} else {
		msg = fmt.Sprint("OK ran job for repository:", repoName, "-", job.Script)
	}

	log.Println(msg)
	return []byte(msg)
}

//////////////////////////////

func main() {

	var relayPort int
	var app string
	var jobConf string

	flag.IntVar(&relayPort, "r", 55555, "Port of Iris relay")
	flag.StringVar(&app, "a", "githook", "Iris app name to register as")
	flag.StringVar(&jobConf, "c", "githook.json", "Job config file (JSON)")
	flag.Parse()

	log.SetPrefix(app)

	conn, err := iris.Connect(relayPort, app, &GithubHandler{jobConf})
	if err != nil {
		log.Fatalf("connection failed: %v.", err)
	}
	defer conn.Close()

	log.Println("Successfully started", app, "using jobConf:", jobConf)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
	<-quit
}
