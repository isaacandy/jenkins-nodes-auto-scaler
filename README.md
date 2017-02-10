Tool that enable and disable build boxes when they're needed.

The tool options are:
```
  -useLocalCreds
    	uses the local creds.json as credentials for Google Cloud APIs
  -workersPerBuildBox int
    	number of workers per build box (default 2)
```

The tool checks the Jenkins queue and when not empty it spins up additional boxes, based on the size of the queue and
 the number of workers per box.
 
When the queue is empty, it checks whether any node can be terminated, by checking if they are idle.

One box is kept running all the time, apart during non working hours (weekends and before 7am or after 7pm).

In case System-Tests Jenkins job is detected to be running, all the pool of boxes is started while deployment to staging 
happen, since we know that we will need all of them in order to run AMP and Legacy system tests.