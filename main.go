package main

import (
	"fmt"
	"log"
	"time"

	"encoding/json"
	"errors"
	"flag"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	"google.golang.org/api/transport"
	"io/ioutil"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
)

type JenkinsQueue struct {
	Items []struct {
		Buildable bool   `json:"buildable"`
		Why       string `json:"why"`
		Task      struct {
			Name string `json:"name"`
		} `json:"task"`
	} `json:"items"`
}

type JenkinsJob struct {
	Color           string `json:"color"`
	NextBuildNumber int    `json:"nextBuildNumber"`
}

type JenkinsBuildBoxInfo struct {
	Idle               bool `json:"idle"`
	TemporarilyOffline bool `json:"temporarilyOffline"`
	Offline            bool `json:"offline"`
	MonitorData        struct {
		HudsonNodeMonitorsArchitectureMonitor *string `json:"hudson.node_monitors.ArchitectureMonitor"`
	} `json:"monitorData"`
}

type systemTest struct {
	sync.Mutex
	running         bool
	nextBuildNumber int
}

var workersPerBuildBox = 2
var buildBoxesPool = []string{"build1-api", "build2-api", "build3-api", "build4-api", "build5-api", "build6-api", "build7-api", "build8-api", "build9-api", "build10-api"}
var systemTestRunning = systemTest{}
var httpClient = &http.Client{}
var service *compute.Service

var lastStarted = struct {
	sync.RWMutex
	m map[string]time.Time
}{m: make(map[string]time.Time)}

func main() {
	defer func() {
		if e := recover(); e != nil {
			log.Printf("\n\033[31;1m%s\x1b[0m\n", e)
			os.Exit(1)
		}
	}()

	workersPerBuildBox = *flag.Int("workersPerBuildBox", workersPerBuildBox, "number of workers per build box")
	localCreds := flag.Bool("useLocalCreds", false, "uses the local creds.json as credentials for Google Cloud APIs")
	jobType := flag.String("jobType", "auto_scaling", "defines which job to execute (auto_scaling, all_up, all_down)")
	flag.Parse()
	if len(flag.Args()) > 0 {
		buildBoxesPool = flag.Args()
	}

	var err error
	if *localCreds {
		service, err = getServiceWithCredsFile()
	} else {
		service, err = getServiceWithDefaultCreds()
	}
	if err != nil {
		log.Printf("Error getting creds: %s\n", err.Error())
		return
	}

	switch *jobType {
	case "all_up":
		enableAllBuildBoxes()
	case "all_down":
		disableAllBuildBoxes()
	default:
		autoScaling()
	}
}

func autoScaling() {
	for {
		queueSize := fetchQueueSize()

		queueSize = adjustQueueSizeDependingWhetherSystemTestIsRunning(queueSize)
		log.Printf("Queue size: %d\n", queueSize)

		if queueSize > 0 {
			enableMoreNodes(queueSize)
		} else {
			disableUnnecessaryBuildBoxes()
		}

		log.Println("Iteration finished")
		fmt.Println("")
		time.Sleep(time.Second * 8)
	}
}

func enableMoreNodes(queueSize int) {
	boxesNeeded := calculateNumberOfNodesToEnable(queueSize)
	log.Println("Checking if any box is offline")
	var wg sync.WaitGroup
	buildBoxesPool = shuffle(buildBoxesPool)
	for _, buildBox := range buildBoxesPool {
		if isNodeOffline(buildBox) {
			wg.Add(1)
			go func(b string) {
				defer wg.Done()
				enableNode(b)
			}(buildBox)
			boxesNeeded = boxesNeeded - 1
			log.Printf("%d more boxes needed\n", boxesNeeded)
		}
		if boxesNeeded <= 0 {
			wg.Wait()
			return
		}
	}
	wg.Wait()
	log.Println("No more build boxes available to start")
}

func shuffle(slice []string) []string {
	for i := range slice {
		j := rand.Intn(i + 1)
		slice[i], slice[j] = slice[j], slice[i]
	}
	return slice
}

func enableNode(buildBox string) bool {
	log.Printf("%s is offline, trying to toggle it online\n", buildBox)
	if !isNodeTemporarilyOffline(buildBox) {
		toggleNodeStatus(buildBox, "offline")
	}
	startCloudBox(buildBox)
	agentLaunched := true
	if !isAgentConnected(buildBox) {
		agentLaunched = launchNodeAgent(buildBox)
	}
	if agentLaunched && isNodeTemporarilyOffline(buildBox) {
		toggleNodeStatus(buildBox, "online")
	}

	return agentLaunched
}

func startCloudBox(buildBox string) {
	if isCloudBoxRunning(buildBox) {
		return
	}

	_, err := service.Instances.Start("service-engineering", "europe-west1-b", buildBox).Do()
	if err != nil {
		log.Println(err)
		return
	}
	waitForStatus(buildBox, "RUNNING")
	lastStarted.Lock()
	lastStarted.m[buildBox] = time.Now()
	lastStarted.Unlock()
}

func calculateNumberOfNodesToEnable(queueSize int) int {
	mod := 0
	if queueSize%workersPerBuildBox != 0 {
		mod = 1
	}

	return (queueSize / workersPerBuildBox) + mod
}

func disableUnnecessaryBuildBoxes() {
	var buildBoxToKeepOnline string
	other := "box"
	if isWorkingHour() {
		buildBoxToKeepOnline = keepOneBoxOnline()
		other = "other box apart from " + buildBoxToKeepOnline
	}

	log.Printf("Checking if any %s is enabled and idle", other)
	var wg sync.WaitGroup
	for _, buildBox := range buildBoxesPool {
		if buildBoxToKeepOnline != buildBox {
			lastStarted.RLock()
			started := lastStarted.m[buildBox]
			lastStarted.RUnlock()
			if !started.IsZero() && started.Add(time.Minute*10).After(time.Now()) {
				log.Printf("%s has not been up for 10 minutes yet", buildBox)
				continue
			}

			wg.Add(1)
			go func(b string) {
				defer wg.Done()
				disableNode(b)
			}(buildBox)
		}
	}
	wg.Wait()
}

func keepOneBoxOnline() string {
	build1BoxPresent := false
	for _, buildBox := range buildBoxesPool {
		if buildBox == "build1-api" {
			build1BoxPresent = true
			break
		}
	}

	var buildBoxToKeepOnline string
	if build1BoxPresent && isCloudBoxRunning("build1-api") && !isNodeOffline("build1-api") && !isNodeTemporarilyOffline("build1-api") {
		buildBoxToKeepOnline = "build1-api"
	} else if build1BoxPresent {
		if enableNode("build1-api") {
			buildBoxToKeepOnline = "build1-api"
		}
	}

	if buildBoxToKeepOnline == "" {
		online := make(chan string, len(buildBoxesPool))
		for _, buildBox := range buildBoxesPool {
			go func(b string, channel chan<- string) {
				if isCloudBoxRunning(b) && !isNodeOffline(b) && !isNodeTemporarilyOffline(b) {
					channel <- b
					return
				}
				channel <- ""
			}(buildBox, online)
		}

		for range buildBoxesPool {
			b := <-online
			if b != "" {
				buildBoxToKeepOnline = b
				log.Printf("Will keep %s online", b)
				break
			}
		}
	}

	if buildBoxToKeepOnline == "" {
		buildBoxToKeepOnline = shuffle(buildBoxesPool)[0]
		log.Printf("Will start %s and keep online", buildBoxToKeepOnline)
		enableNode(buildBoxToKeepOnline)
	}

	return buildBoxToKeepOnline
}

func isWorkingHour() bool {
	location, err := time.LoadLocation("Europe/London")
	if err != nil {
		fmt.Println("Could not load Europe/London location")
		return true
	}

	t := time.Now().In(location)
	if t.Hour() < 7 || t.Hour() > 19 {
		log.Println("Nobody should be working at this time of the day...")
		return false
	}
	if t.Weekday() == 0 || t.Weekday() == 6 {
		log.Println("Nobody should be working on weekends...")
		return false
	}
	return true
}

func disableNode(buildBox string) {
	if !isNodeIdle(buildBox) {
		return
	}

	if !isNodeTemporarilyOffline(buildBox) {
		log.Printf("%s is not offline, trying to toggle it offline\n", buildBox)
		toggleNodeStatus(buildBox, "offline")
	}

	ensureCloudBoxIsNotRunning(buildBox)
}

func toggleNodeStatus(buildBox string, message string) error {
	req, err := http.NewRequest("POST", "http://api-jenkins.shzcld.com/computer/"+buildBox+".c.service-engineering.internal/toggleOffline", nil)
	req.Header.Add("Authorization", "Basic bHVjYS5uYWxkaW5pOmY0MGRkZjI1NGYxOTk0ZWZiMTNjMDc4YjdlMmFmMjJj=")
	resp, err := httpClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		log.Printf("%s was toggled temporarily %s\n", buildBox, message)
	}
	return err
}

func launchNodeAgent(buildBox string) bool {
	log.Printf("Agent was launched for %s, waiting for it to come online\n", buildBox)

	quit := make(chan bool)
	onlineChannel := make(chan bool, 1)
	go func() {
		for {
			select {
			case <-quit:
				return
			default:
				if isAgentConnected(buildBox) {
					onlineChannel <- true
					return
				} else {
					req, _ := http.NewRequest("POST", "http://api-jenkins.shzcld.com/computer/"+buildBox+".c.service-engineering.internal/launchSlaveAgent", nil)
					req.Header.Add("Authorization", "Basic bHVjYS5uYWxkaW5pOmY0MGRkZjI1NGYxOTk0ZWZiMTNjMDc4YjdlMmFmMjJj=")
					resp, err := httpClient.Do(req)
					if err == nil {
						resp.Body.Close()
					}
				}
				time.Sleep(time.Second * 10)
			}
		}
	}()

	agentLaunched := true
	select {
	case <-onlineChannel:
	case <-time.After(time.Second * 120):
		log.Printf("Unable to launch the agent for %s successfully, shutting down", buildBox)
		quit <- true
		agentLaunched = false
		stopCloudBox(buildBox)
	}

	return agentLaunched
}

func stopCloudBox(buildBox string) error {
	_, err := service.Instances.Stop("service-engineering", "europe-west1-b", buildBox).Do()
	if err != nil {
		log.Println(err)
		return err
	}
	waitForStatus(buildBox, "TERMINATED")

	lastStarted.Lock()
	lastStarted.m[buildBox] = time.Time{}
	lastStarted.Unlock()
	return nil
}

func isAgentConnected(buildBox string) bool {
	req, err := http.NewRequest("GET", "http://api-jenkins.shzcld.com/computer/"+buildBox+".c.service-engineering.internal/logText/progressiveHtml", nil)
	req.Header.Add("Authorization", "Basic bHVjYS5uYWxkaW5pOmY0MGRkZjI1NGYxOTk0ZWZiMTNjMDc4YjdlMmFmMjJj=")
	resp, err := httpClient.Do(req)

	if err != nil {
		return false
	}
	defer resp.Body.Close()

	content, _ := ioutil.ReadAll(resp.Body)

	s := string(content)
	if len(s) > 37 && strings.Contains(string(s[len(s)-37:]), "successfully connected and online") {
		return true
	}

	return false
}

func isNodeOffline(buildBox string) bool {
	data := fetchNodeInfo(buildBox)

	return data.Offline
}

func isNodeTemporarilyOffline(buildBox string) bool {
	data := fetchNodeInfo(buildBox)

	return data.TemporarilyOffline
}

func isNodeIdle(buildBox string) bool {
	data := fetchNodeInfo(buildBox)

	return data.Idle
}

func fetchNodeInfo(buildBox string) JenkinsBuildBoxInfo {
	req, err := http.NewRequest("GET", "http://api-jenkins.shzcld.com/computer/"+buildBox+".c.service-engineering.internal/api/json", nil)
	req.Header.Add("Authorization", "Basic bHVjYS5uYWxkaW5pOmY0MGRkZjI1NGYxOTk0ZWZiMTNjMDc4YjdlMmFmMjJj=")
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("Error deserialising Jenkins build box %s info API call: %s\n", buildBox, err.Error())
		return JenkinsBuildBoxInfo{}
	}
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	var data JenkinsBuildBoxInfo
	err = decoder.Decode(&data)

	return data
}

func adjustQueueSizeDependingWhetherSystemTestIsRunning(queueSize int) int {
	systemTestRunning.Lock()
	defer systemTestRunning.Unlock()
	if systemTestRunning.running {
		log.Println("System tests is still running, keep the whole pool enabled")
		return 1
	}
	req, err := http.NewRequest("GET", "http://api-jenkins.shzcld.com/job/System-Tests/api/json", nil)
	req.Header.Add("Authorization", "Basic bHVjYS5uYWxkaW5pOmY0MGRkZjI1NGYxOTk0ZWZiMTNjMDc4YjdlMmFmMjJj=")
	resp, err := httpClient.Do(req)
	if err != nil {
		return queueSize
	}
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	var data JenkinsJob
	err = decoder.Decode(&data)

	if strings.HasSuffix(data.Color, "_anime") && !systemTestRunning.running && systemTestRunning.nextBuildNumber != data.NextBuildNumber {
		systemTestRunning.running = true
		systemTestRunning.nextBuildNumber = data.NextBuildNumber

		go func() {
			time.Sleep(8 * time.Minute)
			systemTestRunning.Lock()
			defer systemTestRunning.Unlock()
			systemTestRunning.running = false
		}()

		log.Println("Detected system tests job, enable the whole pool")
		return workersPerBuildBox * len(buildBoxesPool)
	}

	return queueSize
}

func fetchQueueSize() int {
	req, err := http.NewRequest("GET", "http://api-jenkins.shzcld.com/queue/api/json", nil)
	req.Header.Add("Authorization", "Basic bHVjYS5uYWxkaW5pOmY0MGRkZjI1NGYxOTk0ZWZiMTNjMDc4YjdlMmFmMjJj=")
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("Error deserialising Jenkins queue API call: %s\n", err.Error())
		return 0
	}
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	var data JenkinsQueue
	err = decoder.Decode(&data)
	if err != nil {
		log.Printf("Error deserialising Jenkins queue API call: %s\n", err.Error())
		return 0
	}
	counter := 0
	for _, i := range data.Items {
		if i.Buildable && !strings.HasPrefix(i.Why, "There are no nodes with the label") {
			counter = counter + 1
		}
	}

	return counter
}

func ensureCloudBoxIsNotRunning(buildBox string) {
	if isCloudBoxRunning(buildBox) {
		log.Printf("%s is running... Stopping\n", buildBox)
		stopCloudBox(buildBox)
	}
}

func isCloudBoxRunning(buildBox string) bool {
	i, err := service.Instances.Get("service-engineering", "europe-west1-b", buildBox).Do()
	if nil != err {
		log.Printf("Failed to get instance data: %v\n", err)
		return false
	}

	return i.Status == "RUNNING"
}

func enableAllBuildBoxes() {
	log.Println("Spinning up all build boxes specified")
	var wg sync.WaitGroup
	for _, buildBox := range buildBoxesPool {
		if isNodeOffline(buildBox) {
			wg.Add(1)
			go func(b string) {
				defer wg.Done()
				enableNode(b)
			}(buildBox)
		}
	}
	wg.Wait()
}

func disableAllBuildBoxes() {
	log.Println("Terminating all build boxes specified")
	var wg sync.WaitGroup
	for _, buildBox := range buildBoxesPool {
		wg.Add(1)
		go func(b string) {
			defer wg.Done()
			if !isNodeTemporarilyOffline(b) {
				toggleNodeStatus(b, "offline")
			}

			ensureCloudBoxIsNotRunning(b)
		}(buildBox)
	}
	wg.Wait()
}

func waitForStatus(buildBox string, status string) error {
	previousStatus := ""
	for {
		i, err := service.Instances.Get("service-engineering", "europe-west1-b", buildBox).Do()
		if nil != err {
			log.Printf("Failed to get instance data for %s: %v\n", buildBox, err)
			continue
		}

		if previousStatus != i.Status {
			log.Printf("  %s -> %s\n", buildBox, i.Status)
			previousStatus = i.Status
		}

		if i.Status == status {
			log.Printf("==> %s is %s\n", buildBox, status)
			return nil
		}

		time.Sleep(time.Second * 3)
	}
	return nil
}

func getServiceWithCredsFile() (*compute.Service, error) {
	optionAPIKey := option.WithServiceAccountFile("creds.json")
	if optionAPIKey == nil {
		log.Println("Error creating option.WithAPIKey")
		return nil, errors.New("Error creating option.WithAPIKey")
	}
	optScope := []option.ClientOption{
		option.WithScopes(compute.ComputeScope),
	}
	optionSlice := append(optScope, optionAPIKey)
	ctx := context.TODO()

	httpClient, _, err := transport.NewHTTPClient(ctx, optionSlice...)
	if err != nil {
		log.Printf("Error NewHTTPClient: %s\n", err.Error())
		return nil, err
	}

	service, err := compute.New(httpClient)
	if err != nil {
		log.Printf("Error compute.New(): %s\n", err.Error())
		return nil, err
	}
	return service, nil
}

func getServiceWithDefaultCreds() (*compute.Service, error) {
	ctx := context.TODO()

	client, err := google.DefaultClient(ctx, compute.ComputeScope)
	if err != nil {
		return nil, err
	}
	computeService, err := compute.New(client)
	return computeService, err
}
