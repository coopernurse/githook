package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/coopernurse/cmdlog"
	"github.com/crowdmob/goamz/s3"
	"github.com/karalabe/iris-go"
	"io/ioutil"
	"log"
	"net/http"
	"net/smtp"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
)

type AwsConf struct {
	Access string
	Secret string
	Bucket string
	Region string
	Acl    string
}

type EmailConf struct {
	SmtpHost string
	From     string
	To       []string
	Always   bool
}

type Config struct {
	Repositories map[string]JobDef
	Aws          AwsConf
	Email        EmailConf
	Log          bool
}

func (c Config) SMTPLogger() (cmdlog.SMTPLogger, bool) {
	if c.Email.From != "" && len(c.Email.To) > 0 {
		// TODO: support smtp auth
		auth := smtp.CRAMMD5Auth("", "")

		return cmdlog.SMTPLogger{
			Host: c.Email.SmtpHost,
			From: c.Email.From,
			To:   c.Email.To,
			Auth: &auth,
		}, true
	}
	return cmdlog.SMTPLogger{}, false
}

func (c Config) S3Logger() (cmdlog.S3Logger, bool) {
	if c.Aws.Access != "" && c.Aws.Secret != "" && c.Aws.Bucket != "" {
		bucket, err := cmdlog.EnsureBucket(c.Aws.Access, c.Aws.Secret, c.Aws.Region, c.Aws.Bucket, s3.ACL(c.Aws.Acl))
		if err != nil {
			log.Println("ERROR S3Logger() cannot create bucket for: ", c.Aws, " err: ", err)
		} else {
			return cmdlog.S3Logger{
				Bucket:      bucket,
				Perm:        s3.PublicRead,
				Options:     s3.Options{},
				ContentType: "text/plain",
			}, true
		}
	}
	return cmdlog.S3Logger{}, false
}

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

func toCmd(config Config, repoName string) (*exec.Cmd, error) {
	job, ok := config.Repositories[repoName]
	if !ok {
		return nil, fmt.Errorf("ERROR No config for repository: %s", repoName)
	}

	var cmd *exec.Cmd
	var path string

	if len(job.Script) > 0 {
		path = strings.Replace(job.Script[0], "$dir", job.Dir, 1)
	}

	switch len(job.Script) {
	case 0:
		return nil, fmt.Errorf("ERROR script not defined for repository: %s - %s", repoName, job)
	case 1:
		cmd = exec.Command(path)
	default:
		cmd = exec.Command(path, job.Script[1:]...)
	}

	if job.Dir != "" {
		cmd.Dir = job.Dir
	}

	return cmd, nil
}

func loadConfig(filename string) (Config, error) {
	config := Config{}

	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return config, err
	}

	err = json.Unmarshal(data, &config)
	if err != nil {
		return config, err
	}

	return config, nil
}

func runJob(jobConf string, req GitHubCommitReq) []byte {

	// Load config from JSON file
	config, err := loadConfig(jobConf)
	if err != nil {
		return logMsg(fmt.Sprint("ERROR Unable to load config:", jobConf, " - ", err))
	}

	// Find config for GitHub repository
	repoName := req.Repository.Name
	job, ok := config.Repositories[repoName]
	if !ok {
		return logMsg(fmt.Sprint("ERROR No JobDef for repository:", repoName))
	}

	// Create exec.Cmd based on repo config
	cmd, err := toCmd(config, repoName)
	if err != nil {
		return logMsg(fmt.Sprint("githook: Unable to create command:", err))
	}

	// Run command and time duration
	log.Println("Running job for repository:", repoName, "dir:", cmd.Dir, "script:", cmd.Path)

	go func() {
		result := cmdlog.Run(repoName, cmd)

		if config.Log {
			log.Println("  Status:", result.ExitStr)
			log.Println("  Stdout:", string(result.Stdout))
			log.Println("  Stderr:", string(result.Stderr))
		}

		// Check errors
		sendEmail := config.Email.Always
		var msg, subject string
		if result.Error != nil {
			msg = fmt.Sprint("ERROR Executing job for repository: ", repoName, "-", cmd.Path,
				"-", result.Error, " stdout: ", string(result.Stdout), " stderr: ", string(result.Stderr))
			subject = fmt.Sprintf("Build FAILED for: %s", repoName)
			sendEmail = true
		} else {
			msg = fmt.Sprint("OK ran job for repository: ", repoName, "-", job.Script)
			subject = fmt.Sprintf("Build passed for: %s", repoName)
		}

		// Log result
		resultUrl := ""
		s3logger, ok := config.S3Logger()
		if ok {
			err = s3logger.Log(result)
			if err != nil {
				log.Println("ERROR logging to S3: ", err)
			} else {
				resultUrl = s3logger.URL(result)
			}
		}
		if sendEmail {
			smtpl, ok := config.SMTPLogger()
			if ok {
				err = smtpl.Log(result, subject, resultUrl)
				if err != nil {
					log.Println("ERROR sending email: ", err)
				}
			}
		}

		log.Println(msg)
	}()

	return []byte("Running job for repository:" + repoName)
}

//////////////////////////////

func main() {

	var relayPort int
	var irisApp string
	var jobConf string
	var httpBind string

	flag.IntVar(&relayPort, "r", 55555, "Port of Iris relay")
	flag.StringVar(&irisApp, "a", "", "Iris app name to register as")
	flag.StringVar(&jobConf, "c", "githook.json", "Job config file (JSON)")
	flag.StringVar(&httpBind, "h", "", "HTTP bind addr")
	flag.Parse()

	log.SetPrefix(irisApp)
	ghHandler := &GithubHandler{jobConf}

	if irisApp != "" {
		conn, err := iris.Connect(relayPort, irisApp, ghHandler)
		if err != nil {
			log.Fatalf("connection failed: %v.", err)
		}
		defer conn.Close()
		log.Println("Successfully started", irisApp, "using jobConf:", jobConf)

		quit := make(chan os.Signal, 1)
		signal.Notify(quit, os.Interrupt)
		<-quit
	} else {
		http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
			buf := bytes.Buffer{}
			_, err := buf.ReadFrom(req.Body)
			if err != nil {
				msg := fmt.Sprintf("ReadFrom failed: %v", err)
				log.Println(msg)
				w.Write([]byte(msg))
			} else {
				resp := ghHandler.HandleRequest(buf.Bytes())
				w.Write(resp)
			}
		})

		log.Println("githook listening on:", httpBind, "using jobConf:", jobConf)
		log.Fatal(http.ListenAndServe(httpBind, nil))
	}

}
