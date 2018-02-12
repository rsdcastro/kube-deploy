/*
Copyright 2017 The Kubernetes Authors.

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

package terraform

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"io"
	"os"
	"os/exec"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/golang/glog"
	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"

	tfconfig "k8s.io/kube-deploy/cluster-api-gcp/cloud/terraform/terraformproviderconfig"
	tfconfigv1 "k8s.io/kube-deploy/cluster-api-gcp/cloud/terraform/terraformproviderconfig/v1alpha1"
	apierrors "k8s.io/kube-deploy/cluster-api-gcp/errors"
	"k8s.io/kube-deploy/cluster-api-gcp/util"
	clusterv1 "k8s.io/kube-deploy/cluster-api/api/cluster/v1alpha1"
	"k8s.io/kube-deploy/cluster-api/client"
	apiutil "k8s.io/kube-deploy/cluster-api/util"
)

const (
	ProjectAnnotationKey = "gcp-project"
	ZoneAnnotationKey    = "gcp-zone"
	NameAnnotationKey    = "gcp-name"

	UIDLabelKey       = "machine-crd-uid"
	BootstrapLabelKey = "boostrap"
)

type SshCreds struct {
	user           string
	privateKeyPath string
}

type GCEClient struct {
	service       *compute.Service
	scheme        *runtime.Scheme
	codecFactory  *serializer.CodecFactory
	kubeadmToken  string
	sshCreds      SshCreds
	machineClient client.MachinesInterface
	serializer    *json.Serializer
}

const (
	gceTimeout   = time.Minute * 10
	gceWaitSleep = time.Second * 5
)

func NewMachineActuator(kubeadmToken string, machineClient client.MachinesInterface) (*GCEClient, error) {
	// The default GCP client expects the environment variable
	// GOOGLE_APPLICATION_CREDENTIALS to point to a file with service credentials.
	client, err := google.DefaultClient(context.TODO(), compute.ComputeScope)
	if err != nil {
		return nil, err
	}

	service, err := compute.New(client)
	if err != nil {
		return nil, err
	}

	scheme, codecFactory, err := tfconfigv1.NewSchemeAndCodecs()
	if err != nil {
		return nil, err
	}

	// Only applicable if it's running inside machine controller pod.
	var privateKeyPath, user string
	if _, err := os.Stat("/etc/sshkeys/private"); err == nil {
		privateKeyPath = "/etc/sshkeys/private"

		b, err := ioutil.ReadFile("/etc/sshkeys/user")
		if err == nil {
			user = string(b)
		} else {
			return nil, err
		}
	}

	return &GCEClient{
		service:      service,
		scheme:       scheme,
		codecFactory: codecFactory,
		kubeadmToken: kubeadmToken,
		sshCreds: SshCreds{
			privateKeyPath: privateKeyPath,
			user:           user,
		},
		machineClient: machineClient,
		serializer:    json.NewSerializer(json.DefaultMetaFactory, scheme, scheme, true),
	}, nil
}

func (gce *GCEClient) CreateMachineController(cluster *clusterv1.Cluster, initialMachines []*clusterv1.Machine) error {
	if err := gce.CreateMachineControllerServiceAccount(cluster, initialMachines); err != nil {
		return err
	}

	// Setup SSH access to master VM
	if err := gce.setupSSHAccess(util.GetMaster(initialMachines)); err != nil {
		return err
	}

	if err := CreateMachineControllerPod(gce.kubeadmToken); err != nil {
		return err
	}
	return nil
}

func (gce *GCEClient) Create(cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	config, err := gce.providerconfig(machine.Spec.ProviderConfig)
	if err != nil {
		return gce.handleMachineError(machine, apierrors.InvalidMachineConfiguration(
			"Cannot unmarshal providerConfig field: %v", err))
	}

	name := machine.ObjectMeta.Name
	project := config.Project
	zone := config.Zone
	machine_type := config.MachineType

	temp_dir, err := ioutil.TempDir("/tmp", "cluster-api")
	if err != nil {
	   return err
	}
	copyFile("./cloud/terraform/templates/gcp-instance.tf", temp_dir)

	var_name := fmt.Sprintf("instance_name=%s", name)
	var_zone := fmt.Sprintf("instance_zone=%s", zone)
	var_type := fmt.Sprintf("instance_type=%s", machine_type)
	var_project := fmt.Sprintf("project=%s", project)
	var_uid := fmt.Sprintf("uid=%v", machine.ObjectMeta.UID)

	var metadata map[string]string
	preloaded := false
	if apiutil.IsMaster(machine) {
		if machine.Spec.Versions.ControlPlane == "" {
			return gce.handleMachineError(machine, apierrors.InvalidMachineConfiguration(
				"invalid master configuration: missing Machine.Spec.Versions.ControlPlane"))
		}
		var err error
		metadata, err = masterMetadata(
			templateParams{
				Token:     gce.kubeadmToken,
				Cluster:   cluster,
				Machine:   machine,
				Preloaded: preloaded,
			},
		)
		if err != nil {
			return err
		}
	} else {
		if len(cluster.Status.APIEndpoints) == 0 {
			return errors.New("invalid cluster state: cannot create a Kubernetes node without an API endpoint")
		}
		var err error
		metadata, err = nodeMetadata(
			templateParams{
				Token:     gce.kubeadmToken,
				Cluster:   cluster,
				Machine:   machine,
				Preloaded: preloaded,
			},
		)
		if err != nil {
			return err
		}
	}

	terraformOutputBuffer := bytes.NewBuffer([]byte{})

	cmd := NewCmd(os.Stderr, terraformOutputBuffer)
	err = cmd.Run(os.Stdout, temp_dir, []string{"init"}, true)
	if err != nil {
	   glog.Errorf(fmt.Sprintf("%s", terraformOutputBuffer))
	   return err
	}

	fqin := fmt.Sprintf("%s/%s/%s", project, zone, name)

	terraformOutputBuffer = bytes.NewBuffer([]byte{})
	cmd = NewCmd(os.Stderr, terraformOutputBuffer)
	err = cmd.Run(os.Stdout, temp_dir, []string{"import", "-var", var_project, "google_compute_instance.default", fqin}, true)
	if err != nil {
	   // Import failure is OK if the resource doesn't exist
	   glog.Warningf(fmt.Sprintf("%s", terraformOutputBuffer))
	}

	terraformOutputBuffer = bytes.NewBuffer([]byte{})
	cmd = NewCmd(os.Stderr, terraformOutputBuffer)
	var args []string
	args = append(args, "apply")
	args = append(args, "-auto-approve")
	args = append(args, "-var", var_name)
	args = append(args, "-var", var_zone)
	args = append(args, "-var", var_type)
	args = append(args, "-var", var_project)
	args = append(args, "-var", var_uid)
	args = append(args, "-var", fmt.Sprintf("startup_script=%v}", metadata["startup-script"]))

	err = cmd.Run(os.Stdout, temp_dir, args, true)
	if err != nil {
	   glog.Errorf(fmt.Sprintf("%s", terraformOutputBuffer))
	   return err
	}
	
	return nil
}

func copyFile(file_path string, temp_dir string) {
  from, err := os.Open(file_path)
  if err != nil {
    glog.Fatal(err)
  }
  defer from.Close()

  to, err := os.OpenFile(temp_dir + "/instance.tf", os.O_RDWR|os.O_CREATE, 0666)
  if err != nil {
    glog.Fatal(err)
  }
  defer to.Close()

  _, err = io.Copy(to, from)
  if err != nil {
    glog.Fatal(err)
  }
}

func (gce *GCEClient) Delete(machine *clusterv1.Machine) error {
	glog.Infof("TERRAFORM DELETE.\n")
	return nil
}

func (gce *GCEClient) PostDelete(cluster *clusterv1.Cluster, machines []*clusterv1.Machine) error {
	return gce.DeleteMachineControllerServiceAccount(cluster, machines)
}

func (gce *GCEClient) Update(cluster *clusterv1.Cluster, goalMachine *clusterv1.Machine) error {
	glog.Infof("TERRAFORM UPDATE.\n")
	return nil
}

type Cmd struct {
	stderr       io.Writer
	outputBuffer io.Writer
}

func NewCmd(stderr, outputBuffer io.Writer) Cmd {
	return Cmd{
		stderr:       stderr,
		outputBuffer: outputBuffer,
	}
}

func (c Cmd) Run(stdout io.Writer, workingDirectory string, args []string, debug bool) error {
	command := exec.Command("terraform", args...)
	command.Dir = workingDirectory

	if debug {
		command.Stdout = io.MultiWriter(stdout, c.outputBuffer)
		command.Stderr = io.MultiWriter(c.stderr, c.outputBuffer)
	} else {
		command.Stdout = c.outputBuffer
		command.Stderr = c.outputBuffer
	}

	return command.Run()
}

func (gce *GCEClient) Exists(machine *clusterv1.Machine) (bool, error) {
	i, err := gce.instanceIfExists(machine)
	if err != nil {
		return false, err
	}
	return (i != nil), err
}

func (gce *GCEClient) GetIP(machine *clusterv1.Machine) (string, error) {
	config, err := gce.providerconfig(machine.Spec.ProviderConfig)
	if err != nil {
		return "", err
	}

	instance, err := gce.service.Instances.Get(config.Project, config.Zone, machine.ObjectMeta.Name).Do()
	if err != nil {
		return "", err
	}

	var publicIP string

	for _, networkInterface := range instance.NetworkInterfaces {
		if networkInterface.Name == "nic0" {
			for _, accessConfigs := range networkInterface.AccessConfigs {
				publicIP = accessConfigs.NatIP
			}
		}
	}
	return publicIP, nil
}

func (gce *GCEClient) GetKubeConfig(master *clusterv1.Machine) (string, error) {
	config, err := gce.providerconfig(master.Spec.ProviderConfig)
	if err != nil {
		return "", err
	}

	command := "echo STARTFILE; sudo cat /etc/kubernetes/admin.conf"
	result := strings.TrimSpace(apiutil.ExecCommand(
		"gcloud", "compute", "ssh", "--project", config.Project,
		"--zone", config.Zone, master.ObjectMeta.Name, "--command", command))
	parts := strings.Split(result, "STARTFILE")
	if len(parts) != 2 {
		return "", nil
	}
	return strings.TrimSpace(parts[1]), nil
}

func (gce *GCEClient) updateAnnotations(machine *clusterv1.Machine) error {
	config, err := gce.providerconfig(machine.Spec.ProviderConfig)
	name := machine.ObjectMeta.Name
	project := config.Project
	zone := config.Zone

	if err != nil {
		return gce.handleMachineError(machine,
			apierrors.InvalidMachineConfiguration("Cannot unmarshal providerConfig field: %v", err))
	}

	if machine.ObjectMeta.Annotations == nil {
		machine.ObjectMeta.Annotations = make(map[string]string)
	}
	machine.ObjectMeta.Annotations[ProjectAnnotationKey] = project
	machine.ObjectMeta.Annotations[ZoneAnnotationKey] = zone
	machine.ObjectMeta.Annotations[NameAnnotationKey] = name
	_, err = gce.machineClient.Update(machine)
	if err != nil {
		return err
	}
	err = gce.updateInstanceStatus(machine)
	return err
}

// The two machines differ in a way that requires an update
func (gce *GCEClient) requiresUpdate(a *clusterv1.Machine, b *clusterv1.Machine) bool {
	aConfig, aerr := gce.providerconfig(a.Spec.ProviderConfig)
	bConfig, berr := gce.providerconfig(b.Spec.ProviderConfig)
	if aerr != nil || berr != nil {
		glog.Errorf("Decoding providerconfig failed, aerr=%v, berr=%v", aerr, berr)
		return false
	}

	// Do not want status changes. Do want changes that impact machine provisioning
	return !reflect.DeepEqual(aConfig, bConfig) ||
		!reflect.DeepEqual(a.Spec.ObjectMeta, b.Spec.ObjectMeta) ||
		!reflect.DeepEqual(a.Spec.Roles, b.Spec.Roles) ||
		!reflect.DeepEqual(a.Spec.Versions, b.Spec.Versions) ||
		a.ObjectMeta.Name != b.ObjectMeta.Name ||
		a.ObjectMeta.UID != b.ObjectMeta.UID ||
		!reflect.DeepEqual(aConfig, bConfig)
}

// Gets the instance represented by the given machine
func (gce *GCEClient) instanceIfExists(machine *clusterv1.Machine) (*compute.Instance, error) {
	identifyingMachine := machine

	// Try to use the last saved status locating the machine
	// in case instance details like the proj or zone has changed
	status, err := gce.instanceStatus(machine)
	if err != nil {
		return nil, err
	}

	if status != nil {
		identifyingMachine = (*clusterv1.Machine)(status)
	}

	// Get the VM via specified location and name
	config, err := gce.providerconfig(identifyingMachine.Spec.ProviderConfig)
	if err != nil {
		return nil, err
	}

	instance, err := gce.service.Instances.Get(config.Project, config.Zone, identifyingMachine.ObjectMeta.Name).Do()
	if err != nil {
		// TODO: Use formal way to check for error code 404
		if strings.Contains(err.Error(), "Error 404") {
			return nil, nil
		}
		return nil, err
	}

	uid := instance.Labels[UIDLabelKey]
	if uid == "" {
		if instance.Labels[BootstrapLabelKey] != "" {
			glog.Infof("Skipping uid check since instance %v %v %v is missing uid label due to being provisioned as part of bootstrap.", config.Project, config.Zone, identifyingMachine.ObjectMeta.Name)
		} else {
			return nil, fmt.Errorf("Instance %v %v %v is missing uid label.", config.Project, config.Zone, identifyingMachine.ObjectMeta.Name)
		}
	} else if uid != fmt.Sprintf("%v", machine.ObjectMeta.UID) {
		glog.Infof("Instance %v exists but it has a different UID. Object UID: %v . Instance UID: %v", machine.ObjectMeta.Name, machine.ObjectMeta.UID, uid)
		return nil, nil
	}

	return instance, nil
}

func (gce *GCEClient) providerconfig(providerConfig string) (*tfconfig.TerraformProviderConfig, error) {
	var obj tfconfigv1.TerraformProviderConfig
	_, _, err := gce.serializer.Decode([]byte(providerConfig), &schema.GroupVersionKind{
		Group:   tfconfigv1.GroupName,
		Version: tfconfigv1.VersionName,
		Kind:    reflect.TypeOf(obj).Name()}, &obj)
	if err != nil {
		return nil, fmt.Errorf("decoding failure: %v", err)
	}
	config := tfconfig.TerraformProviderConfig(obj)
	return &config, nil
}

func (gce *GCEClient) waitForOperation(c *tfconfig.TerraformProviderConfig, op *compute.Operation) error {
	glog.Infof("Wait for %v %q...", op.OperationType, op.Name)
	defer glog.Infof("Finish wait for %v %q...", op.OperationType, op.Name)

	start := time.Now()
	ctx, cf := context.WithTimeout(context.Background(), gceTimeout)
	defer cf()

	var err error
	for {
		if err = gce.checkOp(op, err); err != nil || op.Status == "DONE" {
			return err
		}
		glog.V(1).Infof("Wait for %v %q: %v (%d%%): %v", op.OperationType, op.Name, op.Status, op.Progress, op.StatusMessage)
		select {
		case <-ctx.Done():
			return fmt.Errorf("gce operation %v %q timed out after %v", op.OperationType, op.Name, time.Since(start))
		case <-time.After(gceWaitSleep):
		}
		op, err = gce.getOp(c, op)
	}
}

// getOp returns an updated operation.
func (gce *GCEClient) getOp(c *tfconfig.TerraformProviderConfig, op *compute.Operation) (*compute.Operation, error) {
	return gce.service.ZoneOperations.Get(c.Project, path.Base(op.Zone), op.Name).Do()
}

func (gce *GCEClient) checkOp(op *compute.Operation, err error) error {
	if err != nil || op.Error == nil || len(op.Error.Errors) == 0 {
		return err
	}

	var errs bytes.Buffer
	for _, v := range op.Error.Errors {
		errs.WriteString(v.Message)
		errs.WriteByte('\n')
	}
	return errors.New(errs.String())
}

func (gce *GCEClient) updateMasterInplace(oldMachine *clusterv1.Machine, newMachine *clusterv1.Machine) error {
	if oldMachine.Spec.Versions.ControlPlane != newMachine.Spec.Versions.ControlPlane {
		// First pull off the latest kubeadm.
		cmd := "export KUBEADM_VERSION=$(curl -sSL https://dl.k8s.io/release/stable.txt); " +
			"curl -sSL https://dl.k8s.io/release/${KUBEADM_VERSION}/bin/linux/amd64/kubeadm | sudo tee /usr/bin/kubeadm > /dev/null; " +
			"sudo chmod a+rx /usr/bin/kubeadm"
		_, err := gce.remoteSshCommand(newMachine, cmd)
		if err != nil {
			glog.Infof("remotesshcomand error: %v", err)
			return err
		}

		// TODO: We might want to upgrade kubeadm if the target control plane version is newer.
		// Upgrade control plan.
		cmd = fmt.Sprintf("sudo kubeadm upgrade apply %s -y", "v"+newMachine.Spec.Versions.ControlPlane)
		_, err = gce.remoteSshCommand(newMachine, cmd)
		if err != nil {
			glog.Infof("remotesshcomand error: %v", err)
			return err
		}
	}

	// Upgrade kubelet.
	if oldMachine.Spec.Versions.Kubelet != newMachine.Spec.Versions.Kubelet {
		cmd := fmt.Sprintf("sudo kubectl drain %s --kubeconfig /etc/kubernetes/admin.conf --ignore-daemonsets", newMachine.Name)
		// The errors are intentionally ignored as master has static pods.
		gce.remoteSshCommand(newMachine, cmd)
		// Upgrade kubelet to desired version.
		cmd = fmt.Sprintf("sudo apt-get install kubelet=%s", newMachine.Spec.Versions.Kubelet+"-00")
		_, err := gce.remoteSshCommand(newMachine, cmd)
		if err != nil {
			glog.Infof("remotesshcomand error: %v", err)
			return err
		}
		cmd = fmt.Sprintf("sudo kubectl uncordon %s --kubeconfig /etc/kubernetes/admin.conf", newMachine.Name)
		_, err = gce.remoteSshCommand(newMachine, cmd)
		if err != nil {
			glog.Infof("remotesshcomand error: %v", err)
			return err
		}
	}

	return nil
}

func (gce *GCEClient) validateMachine(machine *clusterv1.Machine, config *tfconfig.TerraformProviderConfig) *apierrors.MachineError {
	if machine.Spec.Versions.Kubelet == "" {
		return apierrors.InvalidMachineConfiguration("spec.versions.kubelet can't be empty")
	}
	if machine.Spec.Versions.ContainerRuntime.Name != "docker" {
		return apierrors.InvalidMachineConfiguration("Only docker is supported")
	}
	if machine.Spec.Versions.ContainerRuntime.Version != "1.12.0" {
		return apierrors.InvalidMachineConfiguration("Only docker 1.12.0 is supported")
	}
	return nil
}

// If the GCEClient has a client for updating Machine objects, this will set
// the appropriate reason/message on the Machine.Status. If not, such as during
// cluster installation, it will operate as a no-op. It also returns the
// original error for convenience, so callers can do "return handleMachineError(...)".
func (gce *GCEClient) handleMachineError(machine *clusterv1.Machine, err *apierrors.MachineError) error {
	if gce.machineClient != nil {
		reason := err.Reason
		message := err.Message
		machine.Status.ErrorReason = &reason
		machine.Status.ErrorMessage = &message
		gce.machineClient.Update(machine)
	}

	glog.Errorf("Machine error (terraform): %v", err.Message)
	return err
}

func (gce *GCEClient) getImage(machine *clusterv1.Machine, config *tfconfig.TerraformProviderConfig) (image string, isPreloaded bool) {
	project := config.Project
	imgName := "prebaked-ubuntu-1604-lts"
	fullName := fmt.Sprintf("projects/%s/global/images/%s", project, imgName)

	// Check to see if a preloaded image exists in this project. If so, use it.
	_, err := gce.service.Images.Get(project, imgName).Do()
	if err == nil {
		return fullName, true
	}

	// Otherwise, fall back to the non-preloaded base image.
	return "projects/ubuntu-os-cloud/global/images/family/ubuntu-1604-lts", false
}

// Just a temporary hack to grab a single range from the config.
func getSubnet(netRange clusterv1.NetworkRanges) string {
	if len(netRange.CIDRBlocks) == 0 {
		return ""
	}
	return netRange.CIDRBlocks[0]
}

// Takes a Machine object representing the current state, and update it based on the actual
// current state as reported by GCE, kubelet etc.
func currentMachineActualStatus(gce *GCEClient, currentMachine *clusterv1.Machine) (*clusterv1.Machine, error) {
	instance, err := gce.instanceIfExists(currentMachine)
	if err != nil {
		return nil, err
	}

	machineType := strings.Split(instance.MachineType, "/")
	zone := strings.Split(instance.Zone, "/")

	config, e := gce.providerconfig(currentMachine.Spec.ProviderConfig)
	if e != nil {
		return nil, apierrors.InvalidMachineConfiguration("Cannot unmarshal providerConfig field: %v", e)
	}

	config.MachineType = machineType[len(machineType)-1]
	config.Zone = zone[len(zone)-1]

	b := []byte{}
	buff := bytes.NewBuffer(b)
	err = gce.serializer.Encode(config, buff)
	if err != nil {
		return nil, fmt.Errorf("encoding failure: %v", err)
	}

	currentMachine.Spec.ProviderConfig = buff.String()
	return currentMachine, nil
}
