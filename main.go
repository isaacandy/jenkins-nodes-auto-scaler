package main

import (
	"fmt"
	"log"
	"time"

	"encoding/json"
	"errors"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
	"google.golang.org/api/transport"
	"net/http"
	"os"
)

type JenkinsQueue struct {
	Items []struct {
		Task struct {
			Name string `json:"name"`
		} `json:"task"`
	} `json:"items"`
}

type JenkinsBuildBoxInfo struct {
	Idle               bool `json:"idle"`
	TemporarilyOffline bool `json:"temporarilyOffline"`
	Offline            bool `json:"offline"`
}

const workersPerBuildBox = 2

var buildBoxesPool = []string{"build2-api", "build3-api", "build4-api", "build5-api", "build6-api", "build7-api"}

func main() {
	defer func() {
		if e := recover(); e != nil {
			fmt.Printf("\n\033[31;1m%s\x1b[0m\n", e)
			os.Exit(1)
		}
	}()

	httpClient := &http.Client{}
	service, err := getServiceWithCredsFile()
	if err != nil {
		fmt.Printf("Error getting creds: %s\n", err.Error())
		return
	}

	for {
		queueSize := fetchQueueSize()
		fmt.Printf("Queue size: %d\n", queueSize)

		if queueSize > 0 {
			startBuildBoxes(httpClient, service, queueSize)
		} else {
			stopBuildBoxes(httpClient, service)
		}

		fmt.Println("Iteration finished")
		fmt.Println("")
		time.Sleep(time.Second * 8)
	}

	//res, err := service.Instances.AggregatedList("service-engineering").Filter("(name eq jenkins4-api)").Do()
	//if err != nil {
	//	fmt.Printf("Error getting aggregatedlist: %s\n", err.Error())
	//	return
	//}
	//fmt.Println(res)
}

func startBuildBoxes(httpClient *http.Client, service *compute.Service, queueSize int) {
	boxesNeeded := calculateNumberOfBoxesToStart(queueSize)
	fmt.Println("Checking if any box is offline")
	for _, buildBox := range buildBoxesPool {
		if isNodeTemporaryOffline(buildBox) || isNodeOffline(buildBox) {
			fmt.Printf("%s is offline, trying to toggle it online\n", buildBox)
			startBuildBox(service, buildBox)
			toggleNodeStatus(httpClient, buildBox, "online")
			if isNodeOffline(buildBox) {
				launchNodeAgent(httpClient, buildBox)
			}
			boxesNeeded = boxesNeeded - 1
			fmt.Printf("%d more boxes needed\n", boxesNeeded)
		}
		if boxesNeeded <= 0 {
			return
		}
	}
	fmt.Println("No more build boxes available to start")
}

func startBuildBox(service *compute.Service, buildBox string) {
	_, err := service.Instances.Start("service-engineering", "europe-west1-b", buildBox).Do()
	if err != nil {
		fmt.Println(err)
		return
	}
	waitForStatus(service, buildBox, "RUNNING")
}

func calculateNumberOfBoxesToStart(queueSize int) int {
	mod := 0
	if queueSize%workersPerBuildBox != 0 {
		mod = 1
	}

	return (queueSize / workersPerBuildBox) + mod
}

func stopBuildBoxes(httpClient *http.Client, service *compute.Service) {
	fmt.Println("Checking if any box is enabled and idle")
	for _, buildBox := range buildBoxesPool {
		if isNodeEnabledAndIdle(buildBox) {
			fmt.Printf("%s is enabled and idle, trying to toggle it offline\n", buildBox)
			toggleNodeStatus(httpClient, buildBox, "offline")
			stopBuildBox(service, buildBox)
		} else if isNodeIdle(buildBox) {
			ensureBuildBoxIsNotRunning(service, buildBox)
		}
	}
}

func toggleNodeStatus(httpClient *http.Client, buildBox string, message string) error {
	req, err := http.NewRequest("POST", "http://api-jenkins.shzcld.com/computer/"+buildBox+".c.service-engineering.internal/toggleOffline", nil)
	req.Header.Add("Authorization", "Basic bHVjYS5uYWxkaW5pOmY0MGRkZjI1NGYxOTk0ZWZiMTNjMDc4YjdlMmFmMjJj=")
	_, err = httpClient.Do(req)

	if err == nil {
		fmt.Printf("%s was toggled %s\n", buildBox, message)
	}
	return err
}

func launchNodeAgent(httpClient *http.Client, buildBox string) error {
	req, err := http.NewRequest("POST", "http://api-jenkins.shzcld.com/computer/"+buildBox+".c.service-engineering.internal/launchSlaveAgent", nil)
	req.Header.Add("Authorization", "Basic bHVjYS5uYWxkaW5pOmY0MGRkZjI1NGYxOTk0ZWZiMTNjMDc4YjdlMmFmMjJj=")
	_, err = httpClient.Do(req)

	if err == nil {
		fmt.Printf("Agent was relaunched for %s\n", buildBox)
	}
	return err
}

func stopBuildBox(service *compute.Service, buildBox string) error {
	_, err := service.Instances.Stop("service-engineering", "europe-west1-b", buildBox).Do()
	if err != nil {
		fmt.Println(err)
		return err
	}
	waitForStatus(service, buildBox, "TERMINATED")

	return nil
}

func isNodeOffline(buildBox string) bool {
	data := fetchNodeInfo(buildBox)

	return data.Offline
}

func isNodeTemporaryOffline(buildBox string) bool {
	data := fetchNodeInfo(buildBox)

	return data.TemporarilyOffline
}

func isNodeEnabledAndIdle(buildBox string) bool {
	data := fetchNodeInfo(buildBox)

	return data.Idle && !data.TemporarilyOffline && !data.Offline
}

func isNodeIdle(buildBox string) bool {
	data := fetchNodeInfo(buildBox)

	return data.Idle
}

func fetchNodeInfo(buildBox string) JenkinsBuildBoxInfo {
	resp, err := http.Get("http://api-jenkins.shzcld.com/computer/" + buildBox + ".c.service-engineering.internal/api/json")
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	var data JenkinsBuildBoxInfo
	err = decoder.Decode(&data)
	if err != nil {
		fmt.Printf("Error deserialising Jenkins build box info API call: %s\n", err.Error())
		return JenkinsBuildBoxInfo{}
	}

	return data
}

func fetchQueueSize() int {
	resp, err := http.Get("http://api-jenkins.shzcld.com/queue/api/json")
	defer resp.Body.Close()

	decoder := json.NewDecoder(resp.Body)
	var data JenkinsQueue
	err = decoder.Decode(&data)
	if err != nil {
		fmt.Printf("Error deserialising Jenkins queue API call: %s\n", err.Error())
		return 0
	}

	return len(data.Items)
}

func ensureBuildBoxIsNotRunning(svc *compute.Service, buildBox string) {
	i, err := svc.Instances.Get("service-engineering", "europe-west1-b", buildBox).Do()
	if nil != err {
		log.Printf("Failed to get instance data: %v\n", err)
		return
	}

	if i.Status == "RUNNING" {
		log.Printf("%s was still running, even though node in Jenkins is offline... Stopping\n", buildBox)
		stopBuildBox(svc, buildBox)
	}
}

func waitForStatus(svc *compute.Service, buildBox string, status string) error {
	previousStatus := ""
	for {
		i, err := svc.Instances.Get("service-engineering", "europe-west1-b", buildBox).Do()
		if nil != err {
			log.Printf("Failed to get instance data: %v\n", err)
			continue
		}

		if previousStatus != i.Status {
			log.Printf("  -> %s\n", i.Status)
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
		fmt.Println("Error creating option.WithAPIKey")
		return nil, errors.New("Error creating option.WithAPIKey")
	}
	optScope := []option.ClientOption{
		option.WithScopes(compute.ComputeScope),
	}
	optionSlice := append(optScope, optionAPIKey)
	ctx := context.TODO()

	httpClient, _, err := transport.NewHTTPClient(ctx, optionSlice...)
	if err != nil {
		fmt.Printf("Error NewHTTPClient: %s\n", err.Error())
		return nil, err
	}

	service, err := compute.New(httpClient)
	if err != nil {
		fmt.Printf("Error compute.New(): %s\n", err.Error())
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
