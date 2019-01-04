// Package kubernetes implements Kubernetes operations.
package kubernetes

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-k8s-tester/ec2config"
	"github.com/aws/aws-k8s-tester/internal/ec2"
	"github.com/aws/aws-k8s-tester/internal/etcd"
	"github.com/aws/aws-k8s-tester/internal/ssh"
	"github.com/aws/aws-k8s-tester/kubernetesconfig"
	"github.com/aws/aws-k8s-tester/pkg/awsapi"
	"github.com/aws/aws-k8s-tester/pkg/fileutil"
	"github.com/aws/aws-k8s-tester/pkg/zaputil"
	"github.com/aws/aws-k8s-tester/storagetester"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/aws/aws-sdk-go/service/elb/elbiface"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"github.com/dustin/go-humanize"
	"go.uber.org/zap"
)

// Deployer defines kubernetes test operation.
type Deployer interface {
	Create() error
	Terminate() error
}

type embedded struct {
	mu  sync.RWMutex
	lg  *zap.Logger
	cfg *kubernetesconfig.Config

	etcdTester             storagetester.Tester
	ec2MasterNodesDeployer ec2.Deployer
	ec2WorkerNodesDeployer ec2.Deployer

	ss    *session.Session
	elbv1 elbiface.ELBAPI     // for classic ELB
	elbv2 elbv2iface.ELBV2API // for ALB or NLB
}

// NewDeployer creates a new embedded kubernetes tester.
func NewDeployer(cfg *kubernetesconfig.Config) (Deployer, error) {
	if err := cfg.ValidateAndSetDefaults(); err != nil {
		return nil, err
	}
	lg, err := zaputil.New(cfg.LogDebug, cfg.LogOutputs)
	if err != nil {
		return nil, err
	}
	md := &embedded{lg: lg, cfg: cfg}

	awsCfg := &awsapi.Config{
		Logger:         md.lg,
		DebugAPICalls:  cfg.LogDebug,
		Region:         cfg.AWSRegion,
		CustomEndpoint: "",
	}
	md.ss, err = awsapi.New(awsCfg)
	if err != nil {
		return nil, err
	}
	md.elbv1 = elb.New(md.ss)
	md.elbv2 = elbv2.New(md.ss)

	md.etcdTester, err = etcd.NewTester(md.cfg.ETCDNodes)
	if err != nil {
		return nil, err
	}
	md.ec2MasterNodesDeployer, err = ec2.NewDeployer(md.cfg.EC2MasterNodes)
	if err != nil {
		return nil, err
	}
	md.ec2WorkerNodesDeployer, err = ec2.NewDeployer(md.cfg.EC2WorkerNodes)
	if err != nil {
		return nil, err
	}
	return md, cfg.Sync()
}

// TODO: use EC2 instance profile to do all this?

func (md *embedded) Create() (err error) {
	md.mu.Lock()
	defer md.mu.Unlock()

	now := time.Now().UTC()

	md.cfg.ConfigPathURL = genS3URL(md.cfg.EC2MasterNodes.AWSRegion, md.cfg.Tag, md.cfg.EC2MasterNodes.ConfigPathBucket)
	md.cfg.KubeConfigPathURL = genS3URL(md.cfg.EC2MasterNodes.AWSRegion, md.cfg.Tag, md.cfg.KubeConfigPathBucket)
	md.cfg.LogOutputToUploadPathURL = genS3URL(md.cfg.EC2MasterNodes.AWSRegion, md.cfg.Tag, md.cfg.EC2MasterNodes.LogOutputToUploadPathBucket)

	defer func() {
		if err != nil {
			md.lg.Warn("failed to create Kubernetes, reverting", zap.Error(err))
			md.lg.Warn("failed to create Kubernetes, reverted", zap.Error(md.terminate()))
		}
	}()

	// shared master node VPC and subnets for: etcd nodes, worker nodes
	// do not run this in goroutine, since VPC for master nodes have to be created at first
	os.RemoveAll(md.cfg.EC2MasterNodes.KeyPath)
	if err = md.ec2MasterNodesDeployer.Create(); err != nil {
		return err
	}
	md.cfg.EC2MasterNodesCreated = true
	md.cfg.Sync()

	md.cfg.ETCDNodes.EC2.VPCID = md.cfg.EC2MasterNodes.VPCID
	md.cfg.ETCDNodes.EC2Bastion.VPCID = md.cfg.EC2MasterNodes.VPCID
	md.cfg.EC2WorkerNodes.VPCID = md.cfg.EC2MasterNodes.VPCID

	// prevent VPC double-delete
	md.cfg.ETCDNodes.EC2.VPCCreated = false
	md.cfg.ETCDNodes.EC2Bastion.VPCCreated = false
	md.cfg.EC2WorkerNodes.VPCCreated = false
	md.cfg.Sync()

	errc, ess := make(chan error), make([]string, 0)
	var mu sync.Mutex
	go func() {
		if err = md.etcdTester.Create(); err != nil {
			errc <- fmt.Errorf("failed to create etcd cluster (%v)", err)
			return
		}
		mu.Lock()
		md.cfg.ETCDNodesCreated = true
		mu.Unlock()
		md.cfg.Sync()
		errc <- nil
	}()
	go func() {
		if err = md.ec2WorkerNodesDeployer.Create(); err != nil {
			errc <- fmt.Errorf("failed to create worker nodes (%v)", err)
			return
		}
		mu.Lock()
		md.cfg.EC2WorkerNodesCreated = true
		mu.Unlock()
		md.cfg.Sync()
		errc <- nil
	}()
	for range []int{0, 1} {
		if err = <-errc; err != nil {
			ess = append(ess, err.Error())
		}
	}
	if len(ess) > 0 {
		return errors.New(strings.Join(ess, ", "))
	}

	// TODO: what if >1 master nodes
	var masterNodePrivateDNS string
	for _, v := range md.cfg.EC2MasterNodes.Instances {
		masterNodePrivateDNS = v.PrivateDNSName
	}
	md.cfg.KubeletMasterNodes.HostnameOverride = masterNodePrivateDNS
	md.cfg.KubeProxyMasterNodes.HostnameOverride = masterNodePrivateDNS
	// TODO: what if >2 worker nodes
	var workerNodePrivateDNS string
	for _, v := range md.cfg.EC2WorkerNodes.Instances {
		workerNodePrivateDNS = v.PrivateDNSName
	}
	md.cfg.KubeletWorkerNodes.HostnameOverride = workerNodePrivateDNS
	md.cfg.Sync()

	md.lg.Info(
		"deployed EC2 instances",
		zap.Strings("plugins-master-nodes", md.cfg.EC2MasterNodes.Plugins),
		zap.String("vpc-id-master-nodes", md.cfg.EC2MasterNodes.VPCID),
		zap.Strings("plugins-worker-nodes", md.cfg.EC2WorkerNodes.Plugins),
		zap.String("vpc-id-worker-nodes", md.cfg.EC2WorkerNodes.VPCID),
		zap.String("request-started", humanize.RelTime(now, time.Now().UTC(), "ago", "from now")),
	)
	if md.cfg.LogDebug {
		fmt.Println("EC2MasterNodes.SSHCommands:", md.cfg.EC2MasterNodes.SSHCommands())
		fmt.Println("EC2WorkerNodes.SSHCommands:", md.cfg.EC2WorkerNodes.SSHCommands())
	}
	if err = md.cfg.ValidateAndSetDefaults(); err != nil {
		return err
	}

	var elbOut *elb.CreateLoadBalancerOutput
	elbOut, err = md.elbv1.CreateLoadBalancer(&elb.CreateLoadBalancerInput{
		LoadBalancerName: aws.String(md.cfg.LoadBalancerName),
		SecurityGroups:   aws.StringSlice(md.cfg.EC2MasterNodes.SecurityGroupIDs),
		Subnets:          aws.StringSlice(md.cfg.EC2MasterNodes.SubnetIDs),
		Listeners: []*elb.Listener{
			{
				InstancePort:     aws.Int64(443),
				InstanceProtocol: aws.String("TCP"),
				LoadBalancerPort: aws.Int64(443),
				Protocol:         aws.String("TCP"),
			},
		},
		Tags: []*elb.Tag{
			{Key: aws.String("Name"), Value: aws.String(md.cfg.LoadBalancerName)},
		},
	})
	if err != nil {
		return err
	}
	md.cfg.LoadBalancerCreated = true
	md.cfg.Sync()
	md.lg.Info("created load balancer", zap.String("name", md.cfg.LoadBalancerName), zap.String("dns-name", *elbOut.DNSName))

	instances := make([]*elb.Instance, 0, len(md.cfg.EC2MasterNodes.Instances)+len(md.cfg.EC2WorkerNodes.Instances))
	for _, iv := range md.cfg.EC2MasterNodes.Instances {
		instances = append(instances, &elb.Instance{
			InstanceId: aws.String(iv.InstanceID),
		})
	}
	for _, iv := range md.cfg.EC2WorkerNodes.Instances {
		instances = append(instances, &elb.Instance{
			InstanceId: aws.String(iv.InstanceID),
		})
	}
	if _, err = md.elbv1.RegisterInstancesWithLoadBalancer(&elb.RegisterInstancesWithLoadBalancerInput{
		LoadBalancerName: aws.String(md.cfg.LoadBalancerName),
		Instances:        instances,
	}); err != nil {
		return err
	}
	md.cfg.LoadBalancerRegistered = true
	md.cfg.Sync()
	md.lg.Info("registered instances to load balancer", zap.String("name", md.cfg.LoadBalancerName), zap.Int("instances", len(instances)))

	////////////////////////////////////////////////////////////////////////
	md.lg.Info("step 1-1. downloading 'master node' kubernetes components")
	downloadsMaster := md.cfg.DownloadsMaster()
	errc = make(chan error)
	for _, target := range md.cfg.EC2MasterNodes.Instances {
		go download(md.lg, *md.cfg.EC2MasterNodes, target, downloadsMaster, errc)
	}
	for range md.cfg.EC2MasterNodes.Instances {
		err = <-errc
		if err != nil {
			ess = append(ess, err.Error())
		}
	}
	if len(ess) > 0 {
		return errors.New(strings.Join(ess, ", "))
	}
	md.lg.Info("step 1-2. successfully downloaded 'master node' kubernetes components")
	////////////////////////////////////////////////////////////////////////

	////////////////////////////////////////////////////////////////////////
	md.lg.Info("step 2-1. downloading 'worker node' kubernetes components")
	downloadsWorker := md.cfg.DownloadsWorker()
	for _, target := range md.cfg.EC2WorkerNodes.Instances {
		go download(md.lg, *md.cfg.EC2WorkerNodes, target, downloadsWorker, errc)
	}
	for range md.cfg.EC2WorkerNodes.Instances {
		err = <-errc
		if err != nil {
			ess = append(ess, err.Error())
		}
	}
	if len(ess) > 0 {
		return errors.New(strings.Join(ess, ", "))
	}
	md.lg.Info("step 2-2. successfully downloaded 'worker node' kubernetes components")
	////////////////////////////////////////////////////////////////////////

	////////////////////////////////////////////////////////////////////////
	md.lg.Info("step 3-1. PKI assets")
	md.lg.Info("TODO step 3-2. successfully generated PKI assets")
	////////////////////////////////////////////////////////////////////////

	////////////////////////////////////////////////////////////////////////
	md.lg.Info("step 4-1. 'master node kubelet' configuration")
	md.lg.Info("TODO step 4-2. successfully sent 'master node kubelet' PKI assets")
	md.lg.Info("TODO step 4-3. successfully wrote 'master node kubelet' KUBECONFIG")
	md.lg.Info("TODO step 4-4. successfully sent 'master node kubelet' KUBECONFIG")

	var kubeletEnvMaster string
	if kubeletEnvMaster, err = writeKubeletEnvirontFile(*md.cfg.KubeletMasterNodes); err != nil {
		return err
	}
	defer os.RemoveAll(kubeletEnvMaster)
	md.lg.Info("step 4-5. successfully wrote 'master node kubelet' environment file")

	for _, target := range md.cfg.EC2MasterNodes.Instances {
		if err = sendKubeletEnvFile(md.lg, *md.cfg.EC2MasterNodes, target, kubeletEnvMaster); err != nil {
			return err
		}
	}
	md.lg.Info("step 4-6. successfully sent 'master node kubelet' environment file")

	var kubeletSvcMaster string
	if kubeletSvcMaster, err = writeKubeletServiceFile(*md.cfg.KubeletMasterNodes); err != nil {
		return err
	}
	defer os.RemoveAll(kubeletSvcMaster)
	md.lg.Info("step 4-7. successfully wrote 'master node kubelet' systemd file")

	for _, target := range md.cfg.EC2MasterNodes.Instances {
		if err = sendKubeletServiceFile(md.lg, *md.cfg.EC2MasterNodes, target, kubeletSvcMaster); err != nil {
			return err
		}
	}
	md.lg.Info("step 4-8. successfully sent 'master node kubelet' systemd file")
	////////////////////////////////////////////////////////////////////////

	////////////////////////////////////////////////////////////////////////
	md.lg.Info("step 5-1. 'worker node kubelet' configuration")
	md.lg.Info("TODO step 5-2. successfully sent 'worker node kubelet' PKI assets")
	md.lg.Info("TODO step 5-3. successfully wrote 'worker node kubelet' KUBECONFIG")
	md.lg.Info("TODO step 5-4. successfully sent 'worker node kubelet' KUBECONFIG")

	var kubeletEnvWorker string
	if kubeletEnvWorker, err = writeKubeletEnvirontFile(*md.cfg.KubeletWorkerNodes); err != nil {
		return err
	}
	defer os.RemoveAll(kubeletEnvWorker)
	md.lg.Info("step 5-5. successfully wrote 'worker node kubelet' environment file")

	for _, target := range md.cfg.EC2WorkerNodes.Instances {
		if err = sendKubeletEnvFile(md.lg, *md.cfg.EC2WorkerNodes, target, kubeletEnvWorker); err != nil {
			return err
		}
	}
	md.lg.Info("step 5-6. successfully sent 'worker node kubelet' environment file")

	var kubeletSvcWorker string
	if kubeletSvcWorker, err = writeKubeletServiceFile(*md.cfg.KubeletWorkerNodes); err != nil {
		return err
	}
	defer os.RemoveAll(kubeletSvcWorker)
	md.lg.Info("step 5-7. successfully wrote 'worker node kubelet' systemd file")

	for _, target := range md.cfg.EC2WorkerNodes.Instances {
		if err = sendKubeletServiceFile(md.lg, *md.cfg.EC2WorkerNodes, target, kubeletSvcWorker); err != nil {
			return err
		}
	}
	md.lg.Info("step 5-8. successfully sent 'worker node kubelet' systemd file")
	////////////////////////////////////////////////////////////////////////

	////////////////////////////////////////////////////////////////////////
	md.lg.Info("step 6-1. 'master node kube-proxy' configuration")
	md.lg.Info("TODO step 6-2. successfully sent 'master node kube-proxy' PKI assets")
	md.lg.Info("TODO step 6-3. successfully wrote 'master node kube-proxy' KUBECONFIG")
	md.lg.Info("TODO step 6-4. successfully sent 'master node kube-proxy' KUBECONFIG")
	md.lg.Info("TODO step 6-5. successfully wrote 'master node kube-proxy' environment file")
	md.lg.Info("TODO step 6-6. successfully sent 'master node kube-proxy' environment file")
	md.lg.Info("TODO step 6-7. successfully wrote 'master node kube-proxy' systemd file")
	md.lg.Info("TODO step 6-8. successfully sent 'master node kube-proxy' systemd file")
	////////////////////////////////////////////////////////////////////////

	////////////////////////////////////////////////////////////////////////
	md.lg.Info("step 7-1. 'worker node kube-proxy' configuration")
	md.lg.Info("TODO step 7-2. successfully sent 'worker node kube-proxy' PKI assets")
	md.lg.Info("TODO step 7-3. successfully wrote 'worker node kube-proxy' KUBECONFIG")
	md.lg.Info("TODO step 7-4. successfully sent 'worker node kube-proxy' KUBECONFIG")
	md.lg.Info("TODO step 7-5. successfully wrote 'worker node kube-proxy' environment file")
	md.lg.Info("TODO step 7-6. successfully sent 'worker node kube-proxy' environment file")
	md.lg.Info("TODO step 7-7. successfully wrote 'worker node kube-proxy' systemd file")
	md.lg.Info("TODO step 7-8. successfully sent 'worker node kube-proxy' systemd file")
	////////////////////////////////////////////////////////////////////////

	////////////////////////////////////////////////////////////////////////
	md.lg.Info("step 8-1. 'master node kube-scheduler' configuration")
	md.lg.Info("TODO step 8-2. successfully wrote 'master node kube-scheduler' KUBECONFIG")
	md.lg.Info("TODO step 8-3. successfully sent 'master node kube-scheduler' KUBECONFIG")
	md.lg.Info("TODO step 8-4. successfully wrote 'master node kube-scheduler' environment file")
	md.lg.Info("TODO step 8-5. successfully sent 'master node kube-scheduler' environment file")
	md.lg.Info("TODO step 8-6. successfully wrote 'master node kube-scheduler' systemd file")
	md.lg.Info("TODO step 8-7. successfully sent 'master node kube-scheduler' systemd file")
	////////////////////////////////////////////////////////////////////////

	////////////////////////////////////////////////////////////////////////
	md.lg.Info("step 9-1. 'master node kube-controller-manager' configuration")
	md.lg.Info("TODO step 9-2. successfully sent 'master node kube-controller-manager' PKI assets")
	md.lg.Info("TODO step 9-3. successfully wrote 'master node kube-controller-manager' KUBECONFIG")
	md.lg.Info("TODO step 9-4. successfully sent 'master node kube-controller-manager' KUBECONFIG")
	md.lg.Info("TODO step 9-5. successfully wrote 'master node kube-controller-manager' environment file")
	md.lg.Info("TODO step 9-6. successfully sent 'master node kube-controller-manager' environment file")
	md.lg.Info("TODO step 9-7. successfully wrote 'master node kube-controller-manager' systemd file")
	md.lg.Info("TODO step 9-8. successfully sent 'master node kube-controller-manager' systemd file")
	////////////////////////////////////////////////////////////////////////

	////////////////////////////////////////////////////////////////////////
	md.lg.Info("step 10-1. 'master node kube-apiserver' configuration")
	md.lg.Info("TODO step 10-2. successfully sent 'master node kube-apiserver' PKI assets")
	md.lg.Info("TODO step 10-3. successfully wrote 'master node kube-apiserver' environment file")
	md.lg.Info("TODO step 10-4. successfully sent 'master node kube-apiserver' environment file")
	md.lg.Info("TODO step 10-5. successfully wrote 'master node kube-apiserver' systemd file")
	md.lg.Info("TODO step 10-6. successfully sent 'master node kube-apiserver' systemd file")
	////////////////////////////////////////////////////////////////////////

	////////////////////////////////////////////////////////////////////////
	md.lg.Info("step 11-1. starting master node components")
	md.lg.Info("TODO step 11-2. starting 'master node kubelet'")
	md.lg.Info("TODO step 11-3. successfully started 'master node kubelet'")
	md.lg.Info("TODO step 11-4. starting 'master node kube-proxy'")
	md.lg.Info("TODO step 11-5. successfully started 'master node kube-proxy'")
	md.lg.Info("TODO step 11-6. starting 'master node kube-scheduler'")
	md.lg.Info("TODO step 11-7. successfully started 'master node kube-scheduler'")
	md.lg.Info("TODO step 11-8. starting 'master node kube-controller-manager'")
	md.lg.Info("TODO step 11-9. successfully started 'master node kube-controller-manager'")
	md.lg.Info("TODO step 11-10. starting 'master node kube-apiserver'")
	md.lg.Info("TODO step 11-11. successfully started 'master node kube-apiserver'")
	////////////////////////////////////////////////////////////////////////

	////////////////////////////////////////////////////////////////////////
	md.lg.Info("step 12-1. starting worker node components")
	md.lg.Info("TODO step 12-2. starting 'worker node kubelet'")
	md.lg.Info("TODO step 12-3. successfully started 'worker node kubelet'")
	md.lg.Info("TODO step 12-4. starting 'worker node kube-proxy'")
	md.lg.Info("TODO step 12-5. successfully started 'worker node kube-proxy'")
	////////////////////////////////////////////////////////////////////////

	////////////////////////////////////////////////////////////////////////
	md.lg.Info("step 13-1. 'client-side kubectl' configuration")
	md.lg.Info("TODO step 13-2. successfully wrote 'client-side kubectl' KUBECONFIG")
	md.lg.Info("TODO step 13-3. running 'client-side kubectl' get all")
	md.lg.Info("TODO step 13-4. successfully ran 'client-side kubectl' get all")
	////////////////////////////////////////////////////////////////////////

	if md.cfg.UploadKubeConfig {
		err := md.ec2MasterNodesDeployer.UploadToBucketForTests(md.cfg.KubeConfigPath, md.cfg.KubeConfigPathBucket)
		if err == nil {
			md.lg.Info("uploaded KUBECONFIG", zap.String("path", md.cfg.KubeConfigPath))
		} else {
			md.lg.Warn("failed to upload KUBECONFIG", zap.String("path", md.cfg.KubeConfigPath), zap.Error(err))
		}
	}

	return md.cfg.Sync()
}

/*
sudo systemctl enable kubelet && sudo systemctl restart kubelet
sudo systemctl status kubelet --full --no-pager || true
sudo journalctl --no-pager --output=cat -u kubelet
*/

func (md *embedded) Terminate() error {
	md.mu.Lock()
	defer md.mu.Unlock()
	return md.terminate()
}

func (md *embedded) terminate() error {
	if md.cfg.UploadKubeConfig && md.cfg.EC2MasterNodesCreated {
		err := md.ec2MasterNodesDeployer.UploadToBucketForTests(md.cfg.KubeConfigPath, md.cfg.KubeConfigPathBucket)
		if err == nil {
			md.lg.Info("uploaded KUBECONFIG", zap.String("path", md.cfg.KubeConfigPath))
		} else {
			md.lg.Warn("failed to upload KUBECONFIG", zap.String("path", md.cfg.KubeConfigPath), zap.Error(err))
		}
	}

	md.lg.Info("terminating kubernetes")
	if md.cfg.UploadTesterLogs && len(md.cfg.EC2MasterNodes.Instances) > 0 && md.cfg.EC2MasterNodesCreated {
		fpathToS3PathMasterNodes, err := fetchLogs(
			md.lg,
			md.cfg.EC2MasterNodes.UserName,
			md.cfg.ClusterName,
			md.cfg.EC2MasterNodes.KeyPath,
			md.cfg.EC2MasterNodes.Instances,
		)
		md.cfg.LogsMasterNodes = fpathToS3PathMasterNodes
		if err == nil {
			md.lg.Info("fetched master nodes logs")
		} else {
			md.lg.Warn("failed to fetch master nodes logs", zap.Error(err))
		}
	}

	if md.cfg.UploadTesterLogs && len(md.cfg.EC2WorkerNodes.Instances) > 0 && md.cfg.EC2WorkerNodesCreated {
		fpathToS3PathWorkerNodes, err := fetchLogs(
			md.lg,
			md.cfg.EC2MasterNodes.UserName,
			md.cfg.ClusterName,
			md.cfg.EC2MasterNodes.KeyPath,
			md.cfg.EC2MasterNodes.Instances,
		)
		md.cfg.LogsWorkerNodes = fpathToS3PathWorkerNodes
		if err == nil {
			md.lg.Info("fetched worker nodes logs")
		} else {
			md.lg.Warn("failed to fetch worker nodes logs", zap.Error(err))
		}

		err = md.uploadLogs()
		if err == nil {
			md.lg.Info("uploaded all nodes logs")
		} else {
			md.lg.Warn("failed to upload all nodes logs", zap.Error(err))
		}
	}

	ess := make([]string, 0)

	if md.cfg.LoadBalancerRegistered {
		instances := make([]*elb.Instance, 0, len(md.cfg.EC2MasterNodes.Instances)+len(md.cfg.EC2WorkerNodes.Instances))
		for _, iv := range md.cfg.EC2MasterNodes.Instances {
			instances = append(instances, &elb.Instance{
				InstanceId: aws.String(iv.InstanceID),
			})
		}
		for _, iv := range md.cfg.EC2WorkerNodes.Instances {
			instances = append(instances, &elb.Instance{
				InstanceId: aws.String(iv.InstanceID),
			})
		}
		if _, err := md.elbv1.DeregisterInstancesFromLoadBalancer(&elb.DeregisterInstancesFromLoadBalancerInput{
			LoadBalancerName: aws.String(md.cfg.LoadBalancerName),
			Instances:        instances,
		}); err != nil {
			md.lg.Warn("failed to de-register load balancer", zap.Error(err))
			ess = append(ess, err.Error())
		}
		md.cfg.LoadBalancerRegistered = false
		md.lg.Info("de-registered instances to load balancer", zap.String("name", md.cfg.LoadBalancerName), zap.Int("instances", len(instances)))
	}

	if md.cfg.LoadBalancerCreated {
		if _, err := md.elbv1.DeleteLoadBalancer(&elb.DeleteLoadBalancerInput{
			LoadBalancerName: aws.String(md.cfg.LoadBalancerName),
		}); err != nil {
			md.lg.Warn("failed to delete load balancer", zap.Error(err))
			ess = append(ess, err.Error())
		}
		md.cfg.LoadBalancerCreated = false
		md.lg.Info("deleted load balancer", zap.String("name", md.cfg.LoadBalancerName))
	}

	errc, cnt := make(chan error), 0
	var mu sync.Mutex
	if md.cfg.ETCDNodesCreated {
		cnt++
		// terminate etcd and worker nodes first in order to remove VPC dependency safely
		go func() {
			if err := md.etcdTester.Terminate(); err != nil {
				md.lg.Warn("failed to terminate etcd nodes", zap.Error(err))
				errc <- fmt.Errorf("failed to terminate etcd nodes (%v)", err)
				return
			}
			mu.Lock()
			md.cfg.ETCDNodesCreated = false
			mu.Unlock()
			errc <- nil
		}()
	}

	if md.cfg.EC2WorkerNodesCreated {
		cnt++
		go func() {
			if err := md.ec2WorkerNodesDeployer.Terminate(); err != nil {
				md.lg.Warn("failed to terminate EC2 worker nodes", zap.Error(err))
				errc <- fmt.Errorf("failed to terminate EC2 worker nodes (%v)", err)
				return
			}
			mu.Lock()
			md.cfg.EC2WorkerNodesCreated = false
			mu.Unlock()
			errc <- nil
		}()
	}
	for i := 0; i < cnt; i++ {
		if err := <-errc; err != nil {
			ess = append(ess, err.Error())
		}
	}

	if md.cfg.EC2MasterNodesCreated {
		if err := md.ec2MasterNodesDeployer.Terminate(); err != nil {
			md.lg.Warn("failed to terminate EC2 master nodes", zap.Error(err))
			ess = append(ess, fmt.Sprintf("failed to terminate EC2 master nodes (%v)", err))
		} else {
			md.cfg.EC2MasterNodesCreated = false
		}
	} else {
		md.lg.Warn("master nodes were never created")
	}

	if len(ess) == 0 {
		return nil
	}
	return errors.New(strings.Join(ess, ", "))
}

func (md *embedded) uploadLogs() (err error) {
	ess := make([]string, 0)
	for k, v := range md.cfg.LogsMasterNodes {
		err = md.ec2MasterNodesDeployer.UploadToBucketForTests(k, v)
		if err != nil {
			md.lg.Warn("failed to upload kubernetes master node log", zap.String("file-path", k), zap.Error(err))
			ess = append(ess, err.Error())
		}
	}
	for k, v := range md.cfg.LogsWorkerNodes {
		err = md.ec2WorkerNodesDeployer.UploadToBucketForTests(k, v)
		if err != nil {
			md.lg.Warn("failed to upload kubernetes worker node log", zap.String("file-path", k), zap.Error(err))
			ess = append(ess, err.Error())
		}
	}
	if len(ess) == 0 {
		return nil
	}
	return errors.New(strings.Join(ess, ", "))
}

// TODO: parallelize
func fetchLogs(
	lg *zap.Logger,
	userName string,
	clusterName string,
	privateKeyPath string,
	nodes map[string]ec2config.Instance) (fpathToS3Path map[string]string, err error) {
	fpathToS3Path = make(map[string]string)
	for _, iv := range nodes {
		var fm map[string]string
		fm, err = fetchLog(lg, userName, clusterName, privateKeyPath, iv)
		if err != nil {
			return nil, err
		}
		for k, v := range fm {
			fpathToS3Path[k] = v
		}
	}
	return fpathToS3Path, nil
}

func fetchLog(
	lg *zap.Logger,
	userName string,
	clusterName string,
	privateKeyPath string,
	inst ec2config.Instance) (fpathToS3Path map[string]string, err error) {
	id, ip := inst.InstanceID, inst.PublicIP

	var sh ssh.SSH
	sh, err = ssh.New(ssh.Config{
		Logger:        lg,
		KeyPath:       privateKeyPath,
		UserName:      userName,
		PublicIP:      inst.PublicIP,
		PublicDNSName: inst.PublicDNSName,
	})
	if err != nil {
		lg.Warn(
			"failed to create SSH",
			zap.String("instance-id", id),
			zap.String("public-ip", ip),
			zap.Error(err),
		)
		return nil, err
	}

	if err = sh.Connect(); err != nil {
		lg.Warn(
			"failed to connect",
			zap.String("instance-id", id),
			zap.String("public-ip", ip),
			zap.Error(err),
		)
		return nil, err
	}
	defer sh.Close()

	var out []byte
	out, err = sh.Run(
		"sudo journalctl --no-pager -u kubelet.service",
		ssh.WithRetry(15, 5*time.Second),
		ssh.WithTimeout(15*time.Second),
	)
	if err != nil {
		return nil, err
	}
	var kubeletLogPath string
	kubeletLogPath, err = fileutil.WriteTempFile(out)
	if err != nil {
		return nil, err
	}

	lg.Info("downloaded kubernetes log", zap.String("path", kubeletLogPath))
	fpathToS3Path = make(map[string]string)
	fpathToS3Path[kubeletLogPath] = fmt.Sprintf("%s/%s-kubelet.log", clusterName, id)
	return fpathToS3Path, nil
}

// genS3URL returns S3 URL path.
// e.g. https://s3-us-west-2.amazonaws.com/aws-k8s-tester-20180925/hello-world
func genS3URL(region, bucket, s3Path string) string {
	return fmt.Sprintf("https://s3-%s.amazonaws.com/%s/%s", region, bucket, s3Path)
}
