Tool that enable and disable build boxes according to the current needs.

The tool checks the Jenkins queue and in case is not empty it spins up additional boxes, based on the size of the queue and
 the number of workers per box.

When the queue is empty, it checks whether any node can be terminated by establishing whether the slave is in idle state.

One box is kept running all the time, apart during non working hours.

Once a node is online, it's kept alive for at least 10 minutes, since that's the minimum charge Google applies per node.

The tool assumes:
- all the boxes have the same number of workers configured
- working hours are considered to be between 7am and 7pm, Monday to Friday
- the slave names configured in Jenkins are the same as the node names configured in GCE

The tool options are:
```
  -gceProjectName string
    	project name where nodes are setup in GCE
  -gceZone string
    	GCE zone where nodes have been setup (default "europe-west1-b")
  -jenkinsApiToken string
    	Jenkins api token
  -jenkinsBaseUrl string
    	Jenkins server base url
  -jenkinsUsername string
    	Jenkins username
  -jobNameRequiringAllNodes string
    	Jenkins job name which requires all build nodes enabled
  -jobType string
    	defines which job to execute: auto_scaling, all_up, all_down (default "auto_scaling")
  -locationName string
    	Location used to determine working hours (default "Europe/London")
  -useLocalCreds
    	uses the local creds.json as credentials for Google Cloud APIs
  -workersPerBuildBox int
    	number of workers per build box (default 2)
``` 

![Jenkins nodes setup](/computer.png)

![Jenkins node auto scaler script](/script.png)