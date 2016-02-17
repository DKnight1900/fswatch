package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	ignore "github.com/codeskyblue/dockerignore"
	"github.com/codeskyblue/kexec"
	"github.com/go-fsnotify/fsnotify"
	"github.com/gobuild/log"
	yaml "gopkg.in/yaml.v2"
)

const (
	FWCONFIG_YAML = ".fsw.yml"
	FWCONFIG_JSON = ".fsw.json"
)

var (
	VERSION = "2.0"
)

var signalMaps = map[string]os.Signal{
	"INT":  syscall.SIGINT,
	"HUP":  syscall.SIGHUP,
	"QUIT": syscall.SIGQUIT,
	"TRAP": syscall.SIGTRAP,
	"TERM": syscall.SIGTERM,
	"KILL": syscall.SIGKILL, // kill -9
}

func init() {
	for key, val := range signalMaps {
		signalMaps["SIG"+key] = val
		signalMaps[fmt.Sprintf("%d", val)] = val
	}
	log.SetFlags(0)
	if runtime.GOOS == "windows" {
		log.SetPrefix("fswatch >>> ")
	} else {
		log.SetPrefix("\033[32mfswatch\033[0m >>> ")
	}
}

const (
	CBLACK   = "30"
	CRED     = "31"
	CGREEN   = "32"
	CYELLOW  = "33"
	CBLUE    = "34"
	CMAGENTA = "35"
	CPURPLE  = "36"
)

func CPrintf(ansiColor string, format string, args ...interface{}) {
	if runtime.GOOS != "windows" {
		format = "\033[" + ansiColor + "m" + format + "\033[0m"
	}
	log.Printf(format, args...)
}

type TriggerEvent struct {
	Name          string            `yaml:"name" json:"name"`
	Pattens       []string          `yaml:"pattens" json:"pattens"`
	matchPattens  []string          `yaml:"-" json:"-"`
	Environ       map[string]string `yaml:"env" json:"env"`
	Command       string            `yaml:"cmd" json:"cmd"`
	Delay         string            `yaml:"delay" json:"delay"`
	delayDuration time.Duration     `yaml:"-" json:"-"`
	Signal        string            `yaml:"signal" json:"signal"`
	killSignal    os.Signal         `yaml:"-" json:"-"`
	kcmd          *kexec.KCommand
}

func (this *TriggerEvent) Start() error {
	CPrintf(CGREEN, fmt.Sprintf("[%s] exec start: %s", this.Name, this.Command))
	startTime := time.Now()
	cmd := kexec.CommandString(this.Command)
	env := os.Environ()
	for key, val := range this.Environ {
		env = append(env, fmt.Sprintf("%s=%s", key, val))
	}
	cmd.Env = env
	this.kcmd = cmd
	err := cmd.Start()
	go func() {
		if er := cmd.Wait(); er != nil {
			CPrintf(CRED, "[%s] program exited: %v", this.Name, er)
		}
		log.Infof("[%s] finish in %s", this.Name, time.Since(startTime))
	}()
	return err
}

func (this *TriggerEvent) Stop() {
	if this.kcmd != nil {
		if this.kcmd.ProcessState != nil && this.kcmd.ProcessState.Exited() {
			this.kcmd = nil
			return
		}
		this.kcmd.Terminate(this.killSignal)
		CPrintf(CYELLOW, "[%s] program terminated, signal(%v)", this.Name, this.killSignal)
		this.kcmd = nil
	}
}

// when use func (this *TriggerEvent) strange things happened, wired
func (this *TriggerEvent) WatchEvent(evtC chan FSEvent, wg *sync.WaitGroup) {
	this.Start()
	for evt := range evtC {
		isMatch, err := ignore.Matches(evt.Name, this.Pattens)
		if err != nil {
			log.Fatal(err)
		}
		if !isMatch {
			continue
		}
		this.Stop()
		CPrintf(CGREEN, "changed: %v", evt.Name)
		CPrintf(CGREEN, "delay: %v", this.Delay)
		time.Sleep(this.delayDuration)
		this.Start()
	}
	this.Stop()
	wg.Done()
}

type FSEvent struct {
	Name string
}

type FWConfig struct {
	Description string         `yaml:"desc" json:"desc"`
	Triggers    []TriggerEvent `yaml:"triggers" json:"triggers"`
	WatchPaths  []string       `yaml:"watch_paths" json:"watch_paths"`
	WatchDepth  int            `yaml:"watch_depth" json:"watch_depth"`
}

func fixFWConfig(in FWConfig) (out FWConfig, err error) {
	out = in
	for idx, trigger := range in.Triggers {
		outTg := &out.Triggers[idx]
		if trigger.Delay == "" {
			outTg.Delay = "100ms"
		}
		outTg.delayDuration, err = time.ParseDuration(outTg.Delay)
		if err != nil {
			return
		}
		if outTg.Signal == "" {
			outTg.Signal = "HUP"
		}
		outTg.killSignal = signalMaps[outTg.Signal]

		rd := ioutil.NopCloser(bytes.NewBufferString(strings.Join(outTg.Pattens, "\n")))
		patterns, er := ignore.ReadIgnore(rd)
		if er != nil {
			err = er
			return
		}
		outTg.matchPattens = patterns
	}
	if len(out.WatchPaths) == 0 {
		out.WatchPaths = append(out.WatchPaths, ".")
	}
	if out.WatchDepth == 0 {
		out.WatchDepth = 5
	}

	return
}

func readString(prompt, value string) string {
	fmt.Printf("[?] %s (%s) ", prompt, value)
	var s = value
	fmt.Scanf("%s", &s)
	return s
}

func genFWConfig() FWConfig {
	var (
		name    string
		command string
	)
	cwd, _ := os.Getwd()
	name = filepath.Base(cwd)
	name = readString("name:", name)

	for command == "" {
		command = readString("command:", "go test -v")
	}
	fwc := FWConfig{
		Description: fmt.Sprintf("Auto generated by fswatch [%s]", name),
		Triggers: []TriggerEvent{{
			Pattens: []string{"**/*.go", "**/*.c", "**/*.py"},
			Environ: map[string]string{
				"DEBUG": "1",
			},
			Command: command,
		}},
	}
	out, _ := fixFWConfig(fwc)
	return out
}

func ListAllDir(path string, depth int) (dirs []string, err error) {
	baseNumSeps := strings.Count(path, string(os.PathSeparator))
	err = filepath.Walk(path, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() {
			base := info.Name()
			if base != "." && strings.HasPrefix(base, ".") { // ignore hidden dir
				return filepath.SkipDir
			}

			pathDepth := strings.Count(path, string(os.PathSeparator)) - baseNumSeps
			if pathDepth > depth {
				return filepath.SkipDir
			}
			dirs = append(dirs, path)
		}
		return nil
	})
	return
}

func UniqStrings(ss []string) []string {
	out := make([]string, 0, len(ss))
	m := make(map[string]bool, len(ss))
	for _, key := range ss {
		if !m[key] {
			out = append(out, key)
			m[key] = true
		}
	}
	return out
}

func IsDirectory(path string) bool {
	pinfo, err := os.Stat(path)
	return err == nil && pinfo.IsDir()
}

var fileModifyTimeMap = make(map[string]time.Time)

func IsChanged(path string) bool {
	pinfo, err := os.Stat(path)
	if err != nil {
		return true
	}
	mtime := pinfo.ModTime()
	if mtime.Sub(fileModifyTimeMap[path]) > time.Millisecond*100 { // 100ms
		fileModifyTimeMap[path] = mtime
		return true
	}
	return false
}

// visits here for in case of duplicate paths
func WatchPathAndChildren(w *fsnotify.Watcher, paths []string, depth int, visits map[string]bool) error {
	if visits == nil {
		visits = make(map[string]bool)
	}

	watchDir := func(dir string) {
		if visits[dir] {
			return
		}
		w.Add(dir)
		log.Debug("Watch directory:", dir)
		visits[dir] = true
	}
	var err error
	for _, path := range paths {
		if visits[path] {
			continue
		}

		watchDir(path)
		dirs, er := ListAllDir(path, depth)
		if er != nil {
			err = er
			log.Warnf("ERR list dir: %s, depth: %d, %v", path, depth, err)
			continue
		}

		for _, dir := range dirs {
			watchDir(dir)
		}
	}
	return err
}

func drainEvent(fwc FWConfig) (globalEventC chan FSEvent, wg *sync.WaitGroup, err error) {
	globalEventC = make(chan FSEvent, 1)
	wg = &sync.WaitGroup{}
	evtChannls := make([]chan FSEvent, 0)
	// log.Println(len(fwc.Triggers))
	for _, tg := range fwc.Triggers {
		wg.Add(1)
		evtC := make(chan FSEvent, 1)
		evtChannls = append(evtChannls, evtC)
		go func(tge TriggerEvent) {
			tge.WatchEvent(evtC, wg)
		}(tg)

		// Can't write like this, the next loop tg changed, but go .. is not finished
		// go tg.WatchEvent(evtC, wg)
	}

	go func() {
		for evt := range globalEventC {
			for _, eC := range evtChannls {
				eC <- evt
			}
		}
		for _, eC := range evtChannls {
			close(eC)
		}
	}()
	return
}

func readFWConfig(paths ...string) (fwc FWConfig, err error) {
	for _, cfgPath := range paths {
		data, err := ioutil.ReadFile(cfgPath)
		if err != nil {
			continue
		}
		ext := filepath.Ext(cfgPath)
		switch ext {
		case ".yml":
			if er := yaml.Unmarshal(data, &fwc); er != nil {
				return fwc, er
			}
		case ".json":
			if er := json.Unmarshal(data, &fwc); er != nil {
				return fwc, er
			}
		default:
			err = fmt.Errorf("Unknown format config file: %s", cfgPath)
			return fwc, err
		}
		return fixFWConfig(fwc)
	}
	//fwc, err = fixFWConfig(fwc)
	// data, _ = json.MarshalIndent(fwc, "", "    ")
	// fmt.Println(string(data))
	return fwc, errors.New("Config file not exists")
}

func transformEvent(fsw *fsnotify.Watcher, evtC chan FSEvent) {
	for evt := range fsw.Events {
		if evt.Op == fsnotify.Create && IsDirectory(evt.Name) {
			log.Info("Add watcher", evt.Name)
			fsw.Add(evt.Name)
			continue
		}
		if evt.Op == fsnotify.Remove {
			if err := fsw.Remove(evt.Name); err == nil {
				log.Info("Remove watcher", evt.Name)
			}
			continue
		}
		if !IsChanged(evt.Name) {
			continue
		}
		//log.Printf("Changed: %s", evt.Name)
		evtC <- FSEvent{ // may panic here
			Name: evt.Name,
		}
	}
}

func InitFWConfig() {
	fwc := genFWConfig()
	format := readString("Save format .fsw.(json|yml)", "yml")
	var data []byte
	var cfg string
	if strings.ToLower(format) == "json" {
		data, _ = json.MarshalIndent(fwc, "", "  ")
		cfg = FWCONFIG_JSON
		ioutil.WriteFile(FWCONFIG_JSON, data, 0644)
	} else {
		cfg = FWCONFIG_YAML
		data, _ = yaml.Marshal(fwc)
		ioutil.WriteFile(FWCONFIG_YAML, data, 0644)
	}
	fmt.Printf("Saved to %s\n", strconv.Quote(cfg))
}

func main() {
	version := flag.Bool("version", false, "Show version")
	flag.Parse()

	if *version {
		fmt.Println(VERSION)
		return
	}

	subCmd := flag.Arg(0)
	var fwc FWConfig
	var err error
	if subCmd == "" {
		fwc, err = readFWConfig(FWCONFIG_JSON, FWCONFIG_YAML)
		if err == nil {
			subCmd = "start"
		} else {
			subCmd = "init"
		}
	}

	switch subCmd {
	case "init":
		InitFWConfig()
	case "start":
		visits := make(map[string]bool)
		fsw, _ := fsnotify.NewWatcher()

		err = WatchPathAndChildren(fsw, fwc.WatchPaths, fwc.WatchDepth, visits)
		if err != nil {
			log.Println(err)
		}

		evtC, wg, err := drainEvent(fwc)
		if err != nil {
			log.Fatal(err)
		}

		sigOS := make(chan os.Signal, 1)
		signal.Notify(sigOS, syscall.SIGINT)
		signal.Notify(sigOS, syscall.SIGTERM)

		go func() {
			sig := <-sigOS
			CPrintf(CPURPLE, "Catch signal %v!", sig)
			close(evtC)
		}()
		go transformEvent(fsw, evtC)
		wg.Wait()
		CPrintf(CPURPLE, "Kill all running ... Done")
	}
}
