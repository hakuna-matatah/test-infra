/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/sirupsen/logrus"

	prowapi "k8s.io/test-infra/prow/apis/prowjobs/v1"
	prowjobinformer "k8s.io/test-infra/prow/client/informers/externalversions"
	"k8s.io/test-infra/prow/config"
	"k8s.io/test-infra/prow/config/secret"
	"k8s.io/test-infra/prow/crier"
	gcsreporter "k8s.io/test-infra/prow/crier/reporters/gcs"
	k8sgcsreporter "k8s.io/test-infra/prow/crier/reporters/gcs/kubernetes"
	gerritreporter "k8s.io/test-infra/prow/crier/reporters/gerrit"
	githubreporter "k8s.io/test-infra/prow/crier/reporters/github"
	pubsubreporter "k8s.io/test-infra/prow/crier/reporters/pubsub"
	slackreporter "k8s.io/test-infra/prow/crier/reporters/slack"
	prowflagutil "k8s.io/test-infra/prow/flagutil"
	gerritclient "k8s.io/test-infra/prow/gerrit/client"
	"k8s.io/test-infra/prow/interrupts"
	"k8s.io/test-infra/prow/io"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/logrusutil"
	"k8s.io/test-infra/prow/metrics"
	"k8s.io/test-infra/prow/pjutil"
)

const (
	resync         = 0 * time.Minute
	controllerName = "prow-crier"
)

type options struct {
	client         prowflagutil.KubernetesOptions
	cookiefilePath string
	gerritProjects gerritclient.ProjectsFlag
	github         prowflagutil.GitHubOptions

	configPath    string
	jobConfigPath string

	gerritWorkers         int
	pubsubWorkers         int
	githubWorkers         int
	slackWorkers          int
	gcsWorkers            int
	k8sGCSWorkers         int
	blobStorageWorkers    int
	k8sBlobStorageWorkers int

	slackTokenFile string

	storage prowflagutil.StorageClientOptions

	k8sReportFraction float64

	dryrun      bool
	reportAgent string
}

func (o *options) validate() error {
	if o.configPath == "" {
		return errors.New("required flag --config-path was unset")
	}

	// TODO(krzyzacy): gerrit && github report are actually stateful..
	// Need a better design to re-enable parallel reporting
	if o.gerritWorkers > 1 {
		logrus.Warn("gerrit reporter only supports one worker")
		o.gerritWorkers = 1
	}

	if o.gerritWorkers+o.pubsubWorkers+o.githubWorkers+o.slackWorkers+o.gcsWorkers+o.k8sGCSWorkers+o.blobStorageWorkers+o.k8sBlobStorageWorkers <= 0 {
		return errors.New("crier need to have at least one report worker to start")
	}

	if o.k8sReportFraction < 0 || o.k8sReportFraction > 1 {
		return errors.New("--kubernetes-report-fraction must be a float between 0 and 1")
	}

	if o.gerritWorkers > 0 {
		if len(o.gerritProjects) == 0 {
			return errors.New("--gerrit-projects must be set")
		}

		if o.cookiefilePath == "" {
			logrus.Info("--cookiefile is not set, using anonymous authentication")
		}
	}

	if o.githubWorkers > 0 {
		if err := o.github.Validate(o.dryrun); err != nil {
			return err
		}
	}

	if o.slackWorkers > 0 {
		if o.slackTokenFile == "" {
			return errors.New("--slack-token-file must be set")
		}
	}

	if o.gcsWorkers > 0 {
		// use gcsWorkers if blobStorageWorkers is not set
		if o.blobStorageWorkers == 0 {
			o.blobStorageWorkers = o.gcsWorkers
		}
		logrus.Warn("--gcs-workers is deprecated and will be removed in August 2020. Use --blob-storage-workers instead.")
	}
	if o.k8sGCSWorkers > 0 {
		// use k8sGCSWorkers if k8sBlobStorageWorkers is not set
		if o.k8sBlobStorageWorkers == 0 {
			o.k8sBlobStorageWorkers = o.k8sGCSWorkers
		}
		logrus.Warn("--kubernetes-gcs-workers is deprecated and will be removed in August 2020. Use --kubernetes-blob-storage-workers instead.")
	}

	if err := o.client.Validate(o.dryrun); err != nil {
		return err
	}

	return nil
}

func (o *options) parseArgs(fs *flag.FlagSet, args []string) error {

	o.gerritProjects = gerritclient.ProjectsFlag{}

	fs.StringVar(&o.cookiefilePath, "cookiefile", "", "Path to git http.cookiefile, leave empty for anonymous")
	fs.Var(&o.gerritProjects, "gerrit-projects", "Set of gerrit repos to monitor on a host example: --gerrit-host=https://android.googlesource.com=platform/build,toolchain/llvm, repeat flag for each host")
	fs.IntVar(&o.gerritWorkers, "gerrit-workers", 0, "Number of gerrit report workers (0 means disabled)")
	fs.IntVar(&o.pubsubWorkers, "pubsub-workers", 0, "Number of pubsub report workers (0 means disabled)")
	fs.IntVar(&o.githubWorkers, "github-workers", 0, "Number of github report workers (0 means disabled)")
	fs.IntVar(&o.slackWorkers, "slack-workers", 0, "Number of Slack report workers (0 means disabled)")
	fs.IntVar(&o.gcsWorkers, "gcs-workers", 0, "Number of GCS report workers (0 means disabled)")
	fs.IntVar(&o.k8sGCSWorkers, "kubernetes-gcs-workers", 0, "Number of Kubernetes-specific GCS report workers (0 means disabled)")
	fs.IntVar(&o.blobStorageWorkers, "blob-storage-workers", 0, "Number of blob storage report workers (0 means disabled)")
	fs.IntVar(&o.k8sBlobStorageWorkers, "kubernetes-blob-storage-workers", 0, "Number of Kubernetes-specific blob storage report workers (0 means disabled)")
	fs.Float64Var(&o.k8sReportFraction, "kubernetes-report-fraction", 1.0, "Approximate portion of jobs to report pod information for, if kubernetes-gcs-workers are enabled (0 - > none, 1.0 -> all)")
	fs.StringVar(&o.slackTokenFile, "slack-token-file", "", "Path to a Slack token file")
	fs.StringVar(&o.reportAgent, "report-agent", "", "Only report specified agent - empty means report to all agents (effective for github and Slack only)")

	fs.StringVar(&o.configPath, "config-path", "", "Path to config.yaml.")
	fs.StringVar(&o.jobConfigPath, "job-config-path", "", "Path to prow job configs.")

	// TODO(krzyzacy): implement dryrun for gerrit/pubsub
	fs.BoolVar(&o.dryrun, "dry-run", false, "Run in dry-run mode, not doing actual report (effective for github and Slack only)")

	o.github.AddFlags(fs)
	o.client.AddFlags(fs)
	o.storage.AddFlags(fs)

	fs.Parse(args)

	return o.validate()
}

func parseOptions() options {
	var o options

	if err := o.parseArgs(flag.CommandLine, os.Args[1:]); err != nil {
		logrus.WithError(err).Fatal("Invalid flag options")
	}

	return o
}

func main() {
	logrusutil.ComponentInit()

	o := parseOptions()

	defer interrupts.WaitForGracefulShutdown()

	pjutil.ServePProf()

	configAgent := &config.Agent{}
	if err := configAgent.Start(o.configPath, o.jobConfigPath); err != nil {
		logrus.WithError(err).Fatal("Error starting config agent.")
	}
	cfg := configAgent.Config

	secretAgent := &secret.Agent{}
	if err := secretAgent.Start([]string{}); err != nil {
		logrus.WithError(err).Fatal("unable to start secret agent")
	}

	prowjobClientset, err := o.client.ProwJobClientset(cfg().ProwJobNamespace, o.dryrun)
	if err != nil {
		logrus.WithError(err).Fatal("unable to create prow job client")
	}

	prowjobInformerFactory := prowjobinformer.NewSharedInformerFactoryWithOptions(prowjobClientset, resync, prowjobinformer.WithNamespace(cfg().ProwJobNamespace))

	var controllers []*crier.Controller

	if o.slackWorkers > 0 {
		if cfg().SlackReporter == nil && cfg().SlackReporterConfigs == nil {
			logrus.Fatal("slackreporter is enabled but has no config")
		}
		slackConfig := func(refs *prowapi.Refs) config.SlackReporter {
			return cfg().SlackReporterConfigs.GetSlackReporter(refs)
		}
		if err := secretAgent.Add(o.slackTokenFile); err != nil {
			logrus.WithError(err).Fatal("could not read slack token")
		}
		slackReporter := slackreporter.New(slackConfig, o.dryrun, secretAgent.GetTokenGenerator(o.slackTokenFile))
		controllers = append(
			controllers,
			crier.NewController(
				prowjobClientset,
				kube.RateLimiter(slackReporter.GetName()),
				prowjobInformerFactory.Prow().V1().ProwJobs(),
				slackReporter,
				o.slackWorkers))
	}

	if o.gerritWorkers > 0 {
		informer := prowjobInformerFactory.Prow().V1().ProwJobs()
		gerritReporter, err := gerritreporter.NewReporter(o.cookiefilePath, o.gerritProjects, informer.Lister())
		if err != nil {
			logrus.WithError(err).Fatal("Error starting gerrit reporter")
		}

		controllers = append(
			controllers,
			crier.NewController(
				prowjobClientset,
				kube.RateLimiter(gerritReporter.GetName()),
				informer,
				gerritReporter,
				o.gerritWorkers))
	}

	if o.pubsubWorkers > 0 {
		pubsubReporter := pubsubreporter.NewReporter(cfg)
		controllers = append(
			controllers,
			crier.NewController(
				prowjobClientset,
				kube.RateLimiter(pubsubReporter.GetName()),
				prowjobInformerFactory.Prow().V1().ProwJobs(),
				pubsubReporter,
				o.pubsubWorkers))
	}

	if o.githubWorkers > 0 {
		if o.github.TokenPath != "" {
			if err := secretAgent.Add(o.github.TokenPath); err != nil {
				logrus.WithError(err).Fatal("Error reading GitHub credentials")
			}
		}

		githubClient, err := o.github.GitHubClient(secretAgent, o.dryrun)
		if err != nil {
			logrus.WithError(err).Fatal("Error getting GitHub client.")
		}

		githubReporter := githubreporter.NewReporter(githubClient, cfg, prowapi.ProwJobAgent(o.reportAgent))
		controllers = append(
			controllers,
			crier.NewController(
				prowjobClientset,
				kube.RateLimiter(githubReporter.GetName()),
				prowjobInformerFactory.Prow().V1().ProwJobs(),
				githubReporter,
				o.githubWorkers))
	}

	if o.blobStorageWorkers > 0 || o.k8sBlobStorageWorkers > 0 {
		opener, err := io.NewOpener(context.Background(), o.storage.GCSCredentialsFile, o.storage.S3CredentialsFile)
		if err != nil {
			logrus.WithError(err).Fatal("Error creating opener")
		}

		if o.blobStorageWorkers > 0 {
			gcsReporter := gcsreporter.New(cfg, opener, o.dryrun)
			controllers = append(
				controllers,
				crier.NewController(
					prowjobClientset,
					kube.RateLimiter(gcsReporter.GetName()),
					prowjobInformerFactory.Prow().V1().ProwJobs(),
					gcsReporter,
					o.blobStorageWorkers))
		}

		if o.k8sBlobStorageWorkers > 0 {
			coreClients, err := o.client.BuildClusterCoreV1Clients(o.dryrun)
			if err != nil {
				logrus.WithError(err).Fatal("Error building pod client sets for Kubernetes GCS workers")
			}

			k8sGcsReporter := k8sgcsreporter.New(cfg, opener, coreClients, float32(o.k8sReportFraction), o.dryrun)
			controllers = append(
				controllers,
				crier.NewController(
					prowjobClientset,
					kube.RateLimiter(k8sGcsReporter.GetName()),
					prowjobInformerFactory.Prow().V1().ProwJobs(),
					k8sGcsReporter,
					o.k8sBlobStorageWorkers))
		}
	}

	if len(controllers) == 0 {
		logrus.Fatalf("should have at least one controller to start crier.")
	}

	// Push metrics to the configured prometheus pushgateway endpoint or serve them
	metrics.ExposeMetrics("crier", cfg().PushGateway)

	// run the controller loop to process items
	prowjobInformerFactory.Start(interrupts.Context().Done())
	for i := range controllers {
		controller := controllers[i]
		interrupts.Run(func(ctx context.Context) {
			controller.Run(ctx)
		})
	}
}
