package daemon

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/containers/image/v5/copy"
	"github.com/containers/image/v5/signature"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/opencontainers/image-tools/image"
	kataTypes "github.com/openshift/kata-operator/pkg/apis/kataconfiguration/v1alpha1"
	kataClient "github.com/openshift/kata-operator/pkg/generated/clientset/versioned"
)

// KataInstalled checkes if kata is already installed on the node
type KataInstalled func() (bool, error)

// KataBinaryInstaller installs the kata binaries on the node
type KataBinaryInstaller func() error

//KataOpenShift is used for KataActions on OpenShift cluster nodes
type KataOpenShift struct {
	KataClientSet         kataClient.Interface
	KataInstallChecker    KataInstalled
	kataBinaryInstaller   KataBinaryInstaller
	KataInstallStatusPath string
}

// Install the kata binaries on Openshift
func (k *KataOpenShift) Install(kataConfigResourceName string) error {

	if k.KataInstallStatusPath == "" {
		k.KataInstallStatusPath = "/host/opt/kata-install-daemon/"
	}

	if err := os.MkdirAll(k.KataInstallStatusPath, os.ModePerm); err != nil {
		return err
	}

	if k.KataInstallChecker == nil {
		k.KataInstallChecker = func() (bool, error) {
			var isKataInstalled bool
			var err error
			if _, err := os.Stat("/host/opt/kata-runtime"); err == nil {
				isKataInstalled = true
				err = nil
			} else if os.IsNotExist(err) {
				isKataInstalled = false
				err = nil
			} else {
				isKataInstalled = false
				err = fmt.Errorf("Unknown error while checking kata installation: %+v", err)
			}
			return isKataInstalled, err
		}
	}

	isKataInstalled, err := k.KataInstallChecker()
	if err != nil {
		return err
	}

	if k.kataBinaryInstaller == nil {
		k.kataBinaryInstaller = installRPMs
	}

	if isKataInstalled {
		// kata exist - mark completion
		if _, err := os.Stat(k.KataInstallStatusPath + "/marked_installed"); os.IsNotExist(err) {
			err = updateKataConfigStatus(k.KataClientSet, kataConfigResourceName, func(ks *kataTypes.KataConfigStatus) {
				if ks.InProgressNodesCount > 0 {
					ks.InProgressNodesCount = ks.InProgressNodesCount - 1
				}
				ks.CompletedNodesCount = ks.CompletedNodesCount + 1
			})

			if err != nil {
				return fmt.Errorf("kata exists on the node, error updating kataconfig status %+v", err)
			}

			err = ioutil.WriteFile(k.KataInstallStatusPath+"/marked_installed", []byte(""), 0644)
			if err != nil {
				// TODO - maybe we should roll back the update to kataconfig status above
				return err
			}
		}

	} else {
		// kata doesn't exist, install it.
		err = updateKataConfigStatus(k.KataClientSet, kataConfigResourceName, func(ks *kataTypes.KataConfigStatus) {
			ks.InProgressNodesCount = ks.InProgressNodesCount + 1
		})

		if err != nil {
			return fmt.Errorf("kata is not installed on the node, error updating kataconfig status %+v", err)
		}

		err = k.kataBinaryInstaller()

		// Temporary hold to simulate time taken for the installation of the binaries
		time.Sleep(10 * time.Second)

		if err != nil {
			// kata installation failed. report it.
			err = updateKataConfigStatus(k.KataClientSet, kataConfigResourceName, func(ks *kataTypes.KataConfigStatus) {
				ks.InProgressNodesCount = ks.InProgressNodesCount - 1

				fn, err := getFailedNode(err)
				if err != nil {
					return
				}

				ks.FailedNodes = append(ks.FailedNodes, fn)
			})

			if err != nil {
				return fmt.Errorf("kata installation failed, error updating kataconfig status %+v", err)
			}

		} else {
			// mark daemon completion
			err = updateKataConfigStatus(k.KataClientSet, kataConfigResourceName, func(ks *kataTypes.KataConfigStatus) {
				ks.CompletedDaemons = ks.CompletedDaemons + 1
			})

			if err != nil {
				return fmt.Errorf("kata installation succeeded, but error updating kataconfig status %+v", err)
			}
		}
	}

	return nil
}

// Upgrade the kata binaries and configure the runtime on Openshift
func (k *KataOpenShift) Upgrade() error {
	return fmt.Errorf("Not Implemented Yet")
}

// Uninstall the kata binaries and configure the runtime on Openshift
func (k *KataOpenShift) Uninstall() error {
	return fmt.Errorf("Not Implemented Yet")
}

func doCmd(cmd *exec.Cmd) error {
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	fmt.Println(cmd.String())
	if err != nil {
		log.Println(err)
		return err
	}

	return nil
}

func rpmostreeOverrideReplace(rpms string) error {
	cmd := exec.Command("/bin/bash", "-c", "/usr/bin/rpm-ostree override replace /opt/kata-install/packages/"+rpms)
	err := doCmd(cmd)
	if err != nil {
		fmt.Println("override replace of " + rpms + " failed")
		return err
	}
	return nil
}

func installRPMs() error {
	fmt.Fprintf(os.Stderr, "%s\n", os.Getenv("PATH"))
	log.SetOutput(os.Stdout)

	if _, err := os.Stat("/host/usr/bin/kata-runtime"); err != nil {
		return nil
	}

	cmd := exec.Command("mkdir", "-p", "/host/opt/kata-install")
	err := doCmd(cmd)
	if err != nil {
		return err
	}

	if err := syscall.Chroot("/host"); err != nil {
		log.Fatalf("Unable to chroot to %s: %s", "/host", err)
	}

	if err := syscall.Chdir("/"); err != nil {
		log.Fatalf("Unable to chdir to %s: %s", "/", err)
	}

	policy, err := signature.DefaultPolicy(nil)
	if err != nil {
		fmt.Println(err)
	}
	policyContext, err := signature.NewPolicyContext(policy)
	if err != nil {
		fmt.Println(err)
	}
	srcRef, err := alltransports.ParseImageName("docker://quay.io/jensfr/kata-artifacts:v2.0")
	if err != nil {
		fmt.Println("Invalid source name")
		return err
	}
	destRef, err := alltransports.ParseImageName("oci:/opt/kata-install/kata-image:latest")
	if err != nil {
		fmt.Println("Invalid destination name")
		return err
	}

	_, err = copy.Image(context.Background(), policyContext, destRef, srcRef, &copy.Options{})
	err = image.CreateRuntimeBundleLayout("/opt/kata-install/kata-image/",
		"/usr/local/kata", "latest", "linux", []string{"v1.0"})
	if err != nil {
		fmt.Println("error creating Runtime bundle layout in /usr/local/kata")
		return err
	}

	cmd = exec.Command("mkdir", "-p", "/etc/yum.repos.d/")
	err = doCmd(cmd)
	if err != nil {
		return err
	}

	cmd = exec.Command("/usr/bin/cp", "-f", "/usr/local/kata/linux/packages.repo",
		"/etc/yum.repos.d/")
	if err := doCmd(cmd); err != nil {
		return err
	}

	cmd = exec.Command("/usr/bin/cp", "-f", "/usr/local/kata/linux/katainstall.service",
		"/etc/systemd/system/katainstall.service")
	if err := doCmd(cmd); err != nil {
		return err
	}

	cmd = exec.Command("/usr/bin/cp", "-f",
		"/usr/local/kata/linux/install_kata_packages.sh",
		"/opt/kata-install/install_kata_packages.sh")
	if err := doCmd(cmd); err != nil {
		return err
	}

	cmd = exec.Command("/usr/bin/cp", "-a",
		"/usr/local/kata/linux/packages", "/opt/kata-install/packages")
	if err = doCmd(cmd); err != nil {
		return err
	}

	if err := rpmostreeOverrideReplace("linux-firmware-20191202-97.gite8a0f4c9.el8.noarch.rpm"); err != nil {
		return err
	}

	if err := rpmostreeOverrideReplace("kernel-*.rpm"); err != nil {
		return err
	}
	if err := rpmostreeOverrideReplace("{rdma-core-*.rpm,libibverbs*.rpm}"); err != nil {
		return err
	}

	cmd = exec.Command("/bin/bash", "-c", "/usr/bin/rpm-ostree install --idempotent kata-runtime kata-osbuilder")
	err = doCmd(cmd)
	if err != nil {
		return err
	}

	return nil

}
