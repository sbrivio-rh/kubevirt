package tests_test

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"kubevirt.io/kubevirt/pkg/apimachinery/patch"
	"kubevirt.io/kubevirt/pkg/libdv"
	"kubevirt.io/kubevirt/pkg/libvmi"
	"kubevirt.io/kubevirt/pkg/pointer"

	"kubevirt.io/kubevirt/tests/decorators"
	"kubevirt.io/kubevirt/tests/libkubevirt/config"
	"kubevirt.io/kubevirt/tests/testsuite"

	"kubevirt.io/kubevirt/tests/framework/kubevirt"

	virtsnapshot "kubevirt.io/api/snapshot"
	snapshotv1 "kubevirt.io/api/snapshot/v1beta1"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/format"

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	clone "kubevirt.io/api/clone/v1beta1"
	virtv1 "kubevirt.io/api/core/v1"
	instancetypev1beta1 "kubevirt.io/api/instancetype/v1beta1"
	"kubevirt.io/client-go/kubecli"

	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
	"kubevirt.io/kubevirt/tests/console"
	cd "kubevirt.io/kubevirt/tests/containerdisk"
	. "kubevirt.io/kubevirt/tests/framework/matcher"
	"kubevirt.io/kubevirt/tests/libinstancetype"
	"kubevirt.io/kubevirt/tests/libstorage"
	"kubevirt.io/kubevirt/tests/libvmifact"
)

const (
	vmAPIGroup = "kubevirt.io"
)

var _ = Describe("VirtualMachineClone Tests", Serial, func() {
	var err error
	var virtClient kubecli.KubevirtClient

	BeforeEach(func() {
		virtClient = kubevirt.Client()

		config.EnableFeatureGate(virtconfig.SnapshotGate)

		format.MaxLength = 0
	})

	createSnapshot := func(vm *virtv1.VirtualMachine) *snapshotv1.VirtualMachineSnapshot {
		var err error

		snapshot := &snapshotv1.VirtualMachineSnapshot{
			ObjectMeta: v1.ObjectMeta{
				Name:      "snapshot-" + vm.Name,
				Namespace: vm.Namespace,
			},
			Spec: snapshotv1.VirtualMachineSnapshotSpec{
				Source: k8sv1.TypedLocalObjectReference{
					APIGroup: pointer.P(vmAPIGroup),
					Kind:     "VirtualMachine",
					Name:     vm.Name,
				},
			},
		}

		snapshot, err = virtClient.VirtualMachineSnapshot(snapshot.Namespace).Create(context.Background(), snapshot, v1.CreateOptions{})
		ExpectWithOffset(1, err).ToNot(HaveOccurred())

		return snapshot
	}

	waitSnapshotReady := func(snapshot *snapshotv1.VirtualMachineSnapshot) *snapshotv1.VirtualMachineSnapshot {
		var err error

		EventuallyWithOffset(1, func() bool {
			snapshot, err = virtClient.VirtualMachineSnapshot(snapshot.Namespace).Get(context.Background(), snapshot.Name, v1.GetOptions{})
			ExpectWithOffset(1, err).ToNot(HaveOccurred())
			return snapshot.Status != nil && snapshot.Status.ReadyToUse != nil && *snapshot.Status.ReadyToUse
		}, 180*time.Second, time.Second).Should(BeTrue(), "snapshot should be ready")

		return snapshot
	}

	waitSnapshotContentsExist := func(snapshot *snapshotv1.VirtualMachineSnapshot) *snapshotv1.VirtualMachineSnapshot {
		var contentsName string
		EventuallyWithOffset(1, func() error {
			snapshot, err = virtClient.VirtualMachineSnapshot(snapshot.Namespace).Get(context.Background(), snapshot.Name, v1.GetOptions{})
			ExpectWithOffset(2, err).ToNot(HaveOccurred())
			if snapshot.Status == nil {
				return fmt.Errorf("snapshot's status is nil")
			}

			if snapshot.Status.VirtualMachineSnapshotContentName != nil {
				contentsName = *snapshot.Status.VirtualMachineSnapshotContentName
			} else {
				return fmt.Errorf("vm snapshot contents name is nil")
			}

			return nil
		}, 30*time.Second, 1*time.Second).ShouldNot(HaveOccurred())

		EventuallyWithOffset(1, func() error {
			_, err := virtClient.VirtualMachineSnapshotContent(snapshot.Namespace).Get(context.Background(), contentsName, v1.GetOptions{})
			return err
		}).ShouldNot(HaveOccurred())

		return snapshot
	}

	generateCloneFromVMWithParams := func(sourceVM *virtv1.VirtualMachine, targetVMName string) *clone.VirtualMachineClone {
		vmClone := kubecli.NewMinimalCloneWithNS("testclone", sourceVM.Namespace)

		cloneSourceRef := &k8sv1.TypedLocalObjectReference{
			APIGroup: pointer.P(vmAPIGroup),
			Kind:     "VirtualMachine",
			Name:     sourceVM.Name,
		}

		cloneTargetRef := cloneSourceRef.DeepCopy()
		cloneTargetRef.Name = targetVMName

		vmClone.Spec.Source = cloneSourceRef
		vmClone.Spec.Target = cloneTargetRef

		return vmClone
	}

	generateCloneFromSnapshot := func(snapshot *snapshotv1.VirtualMachineSnapshot, targetVMName string) *clone.VirtualMachineClone {
		vmClone := kubecli.NewMinimalCloneWithNS("testclone", snapshot.Namespace)

		cloneSourceRef := &k8sv1.TypedLocalObjectReference{
			APIGroup: pointer.P(virtsnapshot.GroupName),
			Kind:     "VirtualMachineSnapshot",
			Name:     snapshot.Name,
		}

		cloneTargetRef := &k8sv1.TypedLocalObjectReference{
			APIGroup: pointer.P(vmAPIGroup),
			Kind:     "VirtualMachine",
			Name:     targetVMName,
		}

		vmClone.Spec.Source = cloneSourceRef
		vmClone.Spec.Target = cloneTargetRef

		return vmClone
	}

	createCloneAndWaitForFinish := func(vmClone *clone.VirtualMachineClone) {
		By(fmt.Sprintf("Creating clone object %s", vmClone.Name))
		vmClone, err = virtClient.VirtualMachineClone(vmClone.Namespace).Create(context.Background(), vmClone, v1.CreateOptions{})
		Expect(err).ShouldNot(HaveOccurred())

		By(fmt.Sprintf("Waiting for the clone %s to finish", vmClone.Name))
		Eventually(func() clone.VirtualMachineClonePhase {
			vmClone, err = virtClient.VirtualMachineClone(vmClone.Namespace).Get(context.Background(), vmClone.Name, v1.GetOptions{})
			Expect(err).ShouldNot(HaveOccurred())

			return vmClone.Status.Phase
		}, 3*time.Minute, 3*time.Second).Should(Equal(clone.Succeeded), "clone should finish successfully")
	}

	expectVMRunnable := func(vm *virtv1.VirtualMachine, login console.LoginToFunction) *virtv1.VirtualMachine {
		By(fmt.Sprintf("Starting VM %s", vm.Name))
		vm, err = startCloneVM(virtClient, vm)
		Expect(err).ShouldNot(HaveOccurred())
		Eventually(ThisVM(vm)).WithTimeout(300 * time.Second).WithPolling(time.Second).Should(BeReady())
		targetVMI, err := virtClient.VirtualMachineInstance(vm.Namespace).Get(context.Background(), vm.Name, v1.GetOptions{})
		Expect(err).ShouldNot(HaveOccurred())

		err = login(targetVMI)
		Expect(err).ShouldNot(HaveOccurred())

		vm, err = stopCloneVM(virtClient, vm)
		Expect(err).ShouldNot(HaveOccurred())
		Eventually(ThisVMIWith(vm.Namespace, vm.Name), 300*time.Second, 1*time.Second).ShouldNot(Exist())
		Eventually(ThisVM(vm), 300*time.Second, 1*time.Second).Should(Not(BeReady()))

		return vm
	}

	filterOutIrrelevantKeys := func(in map[string]string) map[string]string {
		out := make(map[string]string)

		for key, val := range in {
			if !strings.Contains(key, "kubevirt.io") && !strings.Contains(key, "kubemacpool.io") {
				out[key] = val
			}
		}

		return out
	}

	Context("VM clone", func() {

		const (
			targetVMName                     = "vm-clone-target"
			cloneShouldEqualSourceMsgPattern = "cloned VM's %s should be equal to source"

			key1   = "key1"
			key2   = "key2"
			value1 = "value1"
			value2 = "value2"
		)

		var (
			sourceVM, targetVM *virtv1.VirtualMachine
			vmClone            *clone.VirtualMachineClone
			defaultVMIOptions  = []libvmi.Option{
				libvmi.WithLabel(key1, value1),
				libvmi.WithLabel(key2, value2),
				libvmi.WithAnnotation(key1, value1),
				libvmi.WithAnnotation(key2, value2),
			}
		)

		expectEqualStrMap := func(actual, expected map[string]string, expectationMsg string, keysToExclude ...string) {
			expected = filterOutIrrelevantKeys(expected)
			actual = filterOutIrrelevantKeys(actual)

			for _, key := range keysToExclude {
				delete(expected, key)
			}

			Expect(actual).To(Equal(expected), expectationMsg)
		}

		expectEqualLabels := func(targetVM, sourceVM *virtv1.VirtualMachine, keysToExclude ...string) {
			expectEqualStrMap(targetVM.Labels, sourceVM.Labels, fmt.Sprintf(cloneShouldEqualSourceMsgPattern, "labels"), keysToExclude...)
		}
		expectEqualTemplateLabels := func(targetVM, sourceVM *virtv1.VirtualMachine, keysToExclude ...string) {
			expectEqualStrMap(targetVM.Spec.Template.ObjectMeta.Labels, sourceVM.Spec.Template.ObjectMeta.Labels, fmt.Sprintf(cloneShouldEqualSourceMsgPattern, "template.labels"), keysToExclude...)
		}

		expectEqualAnnotations := func(targetVM, sourceVM *virtv1.VirtualMachine, keysToExclude ...string) {
			expectEqualStrMap(targetVM.Annotations, sourceVM.Annotations, fmt.Sprintf(cloneShouldEqualSourceMsgPattern, "annotations"), keysToExclude...)
		}
		expectEqualTemplateAnnotations := func(targetVM, sourceVM *virtv1.VirtualMachine, keysToExclude ...string) {
			expectEqualStrMap(targetVM.Spec.Template.ObjectMeta.Annotations, sourceVM.Spec.Template.ObjectMeta.Annotations, fmt.Sprintf(cloneShouldEqualSourceMsgPattern, "template.annotations"), keysToExclude...)
		}

		expectSpecsToEqualExceptForMacAddress := func(vm1, vm2 *virtv1.VirtualMachine) {
			vm1Spec := vm1.Spec.DeepCopy()
			vm2Spec := vm2.Spec.DeepCopy()

			for _, spec := range []*virtv1.VirtualMachineSpec{vm1Spec, vm2Spec} {
				for i := range spec.Template.Spec.Domain.Devices.Interfaces {
					spec.Template.Spec.Domain.Devices.Interfaces[i].MacAddress = ""
				}
			}

			Expect(vm1Spec).To(Equal(vm2Spec), fmt.Sprintf(cloneShouldEqualSourceMsgPattern, "spec not including mac adresses"))
		}

		generateCloneFromVM := func() *clone.VirtualMachineClone {
			return generateCloneFromVMWithParams(sourceVM, targetVMName)
		}

		Context("[sig-compute]simple VM and cloning operations", decorators.SigCompute, func() {

			expectVMRunnable := func(vm *virtv1.VirtualMachine) *virtv1.VirtualMachine {
				return expectVMRunnable(vm, console.LoginToCirros)
			}

			It("simple default clone", func() {
				sourceVM, err = createSourceVM(defaultVMIOptions...)
				Expect(err).ShouldNot(HaveOccurred())
				vmClone = generateCloneFromVM()

				createCloneAndWaitForFinish(vmClone)

				By(fmt.Sprintf("Getting the target VM %s", targetVMName))
				targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(context.Background(), targetVMName, v1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())

				By("Making sure target is runnable")
				targetVM = expectVMRunnable(targetVM)

				Expect(targetVM.Spec).To(Equal(sourceVM.Spec), fmt.Sprintf(cloneShouldEqualSourceMsgPattern, "spec"))
				expectEqualLabels(targetVM, sourceVM)
				expectEqualAnnotations(targetVM, sourceVM)
				expectEqualTemplateLabels(targetVM, sourceVM)
				expectEqualTemplateAnnotations(targetVM, sourceVM)

				By("Making sure snapshot and restore objects are cleaned up")
				Expect(vmClone.Status.SnapshotName).To(BeNil())
				Expect(vmClone.Status.RestoreName).To(BeNil())

				err = virtClient.VirtualMachine(targetVM.Namespace).Delete(context.Background(), targetVM.Name, v1.DeleteOptions{})
				Expect(err).ShouldNot(HaveOccurred())

				Eventually(func() error {
					_, err := virtClient.VirtualMachineClone(vmClone.Namespace).Get(context.Background(), vmClone.Name, v1.GetOptions{})
					return err
				}, 120*time.Second, 1*time.Second).Should(MatchError(errors.IsNotFound, "k8serrors.IsNotFound"), "VM clone should be successfully deleted")
			})

			It("simple clone with snapshot source", func() {
				By("Creating a VM")
				sourceVM, err = createSourceVM(defaultVMIOptions...)
				Expect(err).ShouldNot(HaveOccurred())
				Eventually(func() virtv1.VirtualMachinePrintableStatus {
					sourceVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(context.Background(), sourceVM.Name, v1.GetOptions{})
					Expect(err).ToNot(HaveOccurred())

					return sourceVM.Status.PrintableStatus
				}, 30*time.Second, 1*time.Second).Should(Equal(virtv1.VirtualMachineStatusStopped))

				By("Creating a snapshot from VM")
				snapshot := createSnapshot(sourceVM)
				snapshot = waitSnapshotContentsExist(snapshot)
				// "waitSnapshotReady" is not used here intentionally since it's okay for a snapshot source
				// to not be ready when creating a clone. Therefore, it's not deterministic if snapshot would actually
				// be ready for this test or not.
				// TODO: use snapshot's createDenyVolumeSnapshotCreateWebhook() once it's refactored to work outside
				// of snapshot tests scope.

				By("Deleting VM")
				err = virtClient.VirtualMachine(sourceVM.Namespace).Delete(context.Background(), sourceVM.Name, v1.DeleteOptions{})
				Expect(err).ToNot(HaveOccurred())

				By("Creating a clone with a snapshot source")
				vmClone = generateCloneFromSnapshot(snapshot, targetVMName)
				createCloneAndWaitForFinish(vmClone)

				By(fmt.Sprintf("Getting the target VM %s", targetVMName))
				targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(context.Background(), targetVMName, v1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())

				By("Making sure target is runnable")
				targetVM = expectVMRunnable(targetVM)

				By("Making sure snapshot source is not being deleted")
				_, err = virtClient.VirtualMachineSnapshot(snapshot.Namespace).Get(context.Background(), snapshot.Name, v1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())
			})

			It("clone with only some of labels/annotations", func() {
				sourceVM, err = createSourceVM(defaultVMIOptions...)
				Expect(err).ShouldNot(HaveOccurred())
				vmClone = generateCloneFromVM()

				vmClone.Spec.LabelFilters = []string{
					"*",
					"!" + key2,
				}
				vmClone.Spec.AnnotationFilters = []string{
					key1,
				}
				createCloneAndWaitForFinish(vmClone)

				By(fmt.Sprintf("Getting the target VM %s", targetVMName))
				targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(context.Background(), targetVMName, v1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())

				By("Making sure target is runnable")
				targetVM = expectVMRunnable(targetVM)

				Expect(targetVM.Spec).To(Equal(sourceVM.Spec), fmt.Sprintf(cloneShouldEqualSourceMsgPattern, "spec"))
				expectEqualLabels(targetVM, sourceVM, key2)
				expectEqualAnnotations(targetVM, sourceVM, key2)
			})

			It("clone with only some of template.labels/template.annotations", func() {
				sourceVM, err = createSourceVM(defaultVMIOptions...)
				Expect(err).ShouldNot(HaveOccurred())
				vmClone = generateCloneFromVM()

				vmClone.Spec.Template.LabelFilters = []string{
					"*",
					"!" + key2,
				}
				vmClone.Spec.Template.AnnotationFilters = []string{
					key1,
				}
				createCloneAndWaitForFinish(vmClone)

				By(fmt.Sprintf("Getting the target VM %s", targetVMName))
				targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(context.Background(), targetVMName, v1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())

				By("Making sure target is runnable")
				targetVM = expectVMRunnable(targetVM)

				expectEqualTemplateLabels(targetVM, sourceVM, key2)
				expectEqualTemplateAnnotations(targetVM, sourceVM, key2)
			})

			It("clone with changed MAC address", func() {
				const newMacAddress = "BE-AD-00-00-BE-04"
				options := append(
					defaultVMIOptions,
					libvmi.WithInterface(libvmi.InterfaceDeviceWithMasqueradeBinding()),
					libvmi.WithNetwork(virtv1.DefaultPodNetwork()),
				)
				sourceVM, err = createSourceVM(options...)
				Expect(err).ShouldNot(HaveOccurred())

				srcInterfaces := sourceVM.Spec.Template.Spec.Domain.Devices.Interfaces
				Expect(srcInterfaces).ToNot(BeEmpty())
				srcInterface := srcInterfaces[0]

				vmClone = generateCloneFromVM()
				vmClone.Spec.NewMacAddresses = map[string]string{
					srcInterface.Name: newMacAddress,
				}

				createCloneAndWaitForFinish(vmClone)

				By(fmt.Sprintf("Getting the target VM %s", targetVMName))
				targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(context.Background(), targetVMName, v1.GetOptions{})
				Expect(err).ShouldNot(HaveOccurred())

				By("Making sure target is runnable")
				targetVM = expectVMRunnable(targetVM)

				By("Finding target interface with same name as original")
				var targetInterface *virtv1.Interface
				targetInterfaces := targetVM.Spec.Template.Spec.Domain.Devices.Interfaces
				for _, iface := range targetInterfaces {
					if iface.Name == srcInterface.Name {
						targetInterface = iface.DeepCopy()
						break
					}
				}
				Expect(targetInterface).ToNot(BeNil(), fmt.Sprintf("clone target does not have interface with name %s", srcInterface.Name))

				By("Making sure new mac address is applied to target VM")
				Expect(targetInterface.MacAddress).ToNot(Equal(srcInterface.MacAddress))

				expectSpecsToEqualExceptForMacAddress(targetVM, sourceVM)
				expectEqualLabels(targetVM, sourceVM)
				expectEqualAnnotations(targetVM, sourceVM)
				expectEqualTemplateLabels(targetVM, sourceVM)
				expectEqualTemplateAnnotations(targetVM, sourceVM)
			})

			Context("regarding domain Firmware", func() {
				It("clone with changed SMBios serial", func() {
					const sourceSerial = "source-serial"
					const targetSerial = "target-serial"

					options := append(
						defaultVMIOptions,
						withFirmware(&virtv1.Firmware{Serial: sourceSerial}),
					)
					sourceVM, err = createSourceVM(options...)
					Expect(err).ShouldNot(HaveOccurred())

					vmClone = generateCloneFromVM()
					vmClone.Spec.NewSMBiosSerial = pointer.P(targetSerial)

					createCloneAndWaitForFinish(vmClone)

					By(fmt.Sprintf("Getting the target VM %s", targetVMName))
					targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(context.Background(), targetVMName, v1.GetOptions{})
					Expect(err).ShouldNot(HaveOccurred())

					By("Making sure target is runnable")
					targetVM = expectVMRunnable(targetVM)

					By("Making sure new smBios serial is applied to target VM")
					Expect(targetVM.Spec.Template.Spec.Domain.Firmware).ToNot(BeNil())
					Expect(sourceVM.Spec.Template.Spec.Domain.Firmware).ToNot(BeNil())
					Expect(targetVM.Spec.Template.Spec.Domain.Firmware.Serial).ToNot(Equal(sourceVM.Spec.Template.Spec.Domain.Firmware.Serial))

					expectEqualLabels(targetVM, sourceVM)
					expectEqualAnnotations(targetVM, sourceVM)
					expectEqualTemplateLabels(targetVM, sourceVM)
					expectEqualTemplateAnnotations(targetVM, sourceVM)
				})

				It("should strip firmware UUID", func() {
					const fakeFirmwareUUID = "fake-uuid"

					options := append(
						defaultVMIOptions,
						withFirmware(&virtv1.Firmware{UUID: fakeFirmwareUUID}),
					)
					sourceVM, err = createSourceVM(options...)
					Expect(err).ShouldNot(HaveOccurred())
					vmClone = generateCloneFromVM()

					createCloneAndWaitForFinish(vmClone)

					By(fmt.Sprintf("Getting the target VM %s", targetVMName))
					targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(context.Background(), targetVMName, v1.GetOptions{})
					Expect(err).ShouldNot(HaveOccurred())

					By("Making sure target is runnable")
					targetVM = expectVMRunnable(targetVM)

					By("Making sure new smBios serial is applied to target VM")
					Expect(targetVM.Spec.Template.Spec.Domain.Firmware).ToNot(BeNil())
					Expect(sourceVM.Spec.Template.Spec.Domain.Firmware).ToNot(BeNil())
					Expect(targetVM.Spec.Template.Spec.Domain.Firmware.UUID).ToNot(Equal(sourceVM.Spec.Template.Spec.Domain.Firmware.UUID))
				})
			})

		})

		Context("[sig-storage]with more complicated VM", decorators.SigStorage, func() {

			expectVMRunnable := func(vm *virtv1.VirtualMachine) *virtv1.VirtualMachine {
				return expectVMRunnable(vm, console.LoginToAlpine)
			}

			createVMWithStorageClass := func(storageClass string, runStrategy virtv1.VirtualMachineRunStrategy) *virtv1.VirtualMachine {
				dv := libdv.NewDataVolume(
					libdv.WithRegistryURLSource(cd.DataVolumeImportUrlForContainerDisk(cd.ContainerDiskAlpine)),
					libdv.WithNamespace(testsuite.GetTestNamespace(nil)),
					libdv.WithStorage(
						libdv.StorageWithStorageClass(storageClass),
						libdv.StorageWithVolumeSize(cd.ContainerDiskSizeBySourceURL(cd.DataVolumeImportUrlForContainerDisk(cd.ContainerDiskAlpine))),
					),
				)
				vm := libstorage.RenderVMWithDataVolumeTemplate(dv)
				vm.Spec.RunStrategy = &runStrategy
				vm, err := virtClient.VirtualMachine(vm.Namespace).Create(context.Background(), vm, v1.CreateOptions{})
				Expect(err).ToNot(HaveOccurred())

				if !(runStrategy == virtv1.RunStrategyAlways) && libstorage.IsStorageClassBindingModeWaitForFirstConsumer(storageClass) {
					return vm
				}

				for _, dvt := range vm.Spec.DataVolumeTemplates {
					libstorage.EventuallyDVWith(vm.Namespace, dvt.Name, 180, HaveSucceeded())
				}

				return vm
			}

			Context("and no snapshot storage class", decorators.RequiresNoSnapshotStorageClass, func() {
				var (
					noSnapshotStorageClass string
				)

				Context("should reject source with non snapshotable volume", func() {
					BeforeEach(func() {
						noSnapshotStorageClass = libstorage.GetNoVolumeSnapshotStorageClass("local")
						Expect(noSnapshotStorageClass).ToNot(BeEmpty(), "no storage class without snapshot support")

						// create running in case storage is WFFC (local storage)
						By("Creating source VM")
						sourceVM = createVMWithStorageClass(noSnapshotStorageClass, virtv1.RunStrategyAlways)
						sourceVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(context.Background(), sourceVM.Name, v1.GetOptions{})
						Expect(err).ToNot(HaveOccurred())
						sourceVM, err = stopCloneVM(virtClient, sourceVM)
						Eventually(ThisVMIWith(sourceVM.Namespace, sourceVM.Name), 300*time.Second, 1*time.Second).ShouldNot(Exist())
						Eventually(ThisVM(sourceVM), 300*time.Second, 1*time.Second).Should(Not(BeReady()))
					})

					It("with VM source", func() {
						vmClone = generateCloneFromVM()
						vmClone, err = virtClient.VirtualMachineClone(vmClone.Namespace).Create(context.Background(), vmClone, v1.CreateOptions{})
						Expect(err).To(HaveOccurred())
						Expect(err.Error()).Should(ContainSubstring("does not support snapshots"))
					})

					It("with snapshot source", func() {
						By("Snapshotting VM")
						snapshot := createSnapshot(sourceVM)
						snapshot = waitSnapshotReady(snapshot)

						By("Deleting VM")
						err = virtClient.VirtualMachine(sourceVM.Namespace).Delete(context.Background(), sourceVM.Name, v1.DeleteOptions{})
						Expect(err).ToNot(HaveOccurred())

						By("Creating a clone and expecting error")
						vmClone = generateCloneFromSnapshot(snapshot, targetVMName)
						vmClone, err = virtClient.VirtualMachineClone(vmClone.Namespace).Create(context.Background(), vmClone, v1.CreateOptions{})
						Expect(err).Should(HaveOccurred())
						Expect(err.Error()).To(ContainSubstring("not backed up in snapshot"))
					})
				})
			})

			Context("and snapshot storage class", decorators.RequiresSnapshotStorageClass, func() {
				var (
					snapshotStorageClass string
				)

				BeforeEach(func() {
					snapshotStorageClass, err = libstorage.GetSnapshotStorageClass(virtClient)
					Expect(err).ToNot(HaveOccurred())
					Expect(snapshotStorageClass).ToNot(BeEmpty(), "no storage class with snapshot support")
				})

				It("with a simple clone", func() {
					runStrategy := virtv1.RunStrategyHalted
					if libstorage.IsStorageClassBindingModeWaitForFirstConsumer(snapshotStorageClass) {
						// with wffc need to start the virtual machine
						// in order for the pvc to be populated
						runStrategy = virtv1.RunStrategyAlways
					}
					sourceVM = createVMWithStorageClass(snapshotStorageClass, runStrategy)
					vmClone = generateCloneFromVM()

					createCloneAndWaitForFinish(vmClone)

					By(fmt.Sprintf("Getting the target VM %s", targetVMName))
					targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(context.Background(), targetVMName, v1.GetOptions{})
					Expect(err).ShouldNot(HaveOccurred())

					By("Making sure target is runnable")
					targetVM = expectVMRunnable(targetVM)

					expectEqualLabels(targetVM, sourceVM)
					expectEqualAnnotations(targetVM, sourceVM)
					expectEqualTemplateLabels(targetVM, sourceVM)
					expectEqualTemplateAnnotations(targetVM, sourceVM)
				})

				Context("with instancetype and preferences", func() {
					var (
						instancetype *instancetypev1beta1.VirtualMachineInstancetype
						preference   *instancetypev1beta1.VirtualMachinePreference
					)

					BeforeEach(func() {
						ns := testsuite.GetTestNamespace(nil)
						instancetype = &instancetypev1beta1.VirtualMachineInstancetype{
							ObjectMeta: v1.ObjectMeta{
								GenerateName: "vm-instancetype-",
								Namespace:    ns,
							},
							Spec: instancetypev1beta1.VirtualMachineInstancetypeSpec{
								CPU: instancetypev1beta1.CPUInstancetype{
									Guest: 1,
								},
								Memory: instancetypev1beta1.MemoryInstancetype{
									Guest: resource.MustParse("128Mi"),
								},
							},
						}
						instancetype, err := virtClient.VirtualMachineInstancetype(ns).Create(context.Background(), instancetype, v1.CreateOptions{})
						Expect(err).ToNot(HaveOccurred())

						preferredCPUTopology := instancetypev1beta1.Sockets
						preference = &instancetypev1beta1.VirtualMachinePreference{
							ObjectMeta: v1.ObjectMeta{
								GenerateName: "vm-preference-",
								Namespace:    ns,
							},
							Spec: instancetypev1beta1.VirtualMachinePreferenceSpec{
								CPU: &instancetypev1beta1.CPUPreferences{
									PreferredCPUTopology: &preferredCPUTopology,
								},
							},
						}
						preference, err := virtClient.VirtualMachinePreference(ns).Create(context.Background(), preference, v1.CreateOptions{})
						Expect(err).ToNot(HaveOccurred())

						dv := libdv.NewDataVolume(
							libdv.WithRegistryURLSource(cd.DataVolumeImportUrlForContainerDisk(cd.ContainerDiskAlpine)),
							libdv.WithNamespace(testsuite.GetTestNamespace(nil)),
							libdv.WithStorage(
								libdv.StorageWithStorageClass(snapshotStorageClass),
								libdv.StorageWithVolumeSize(cd.ContainerDiskSizeBySourceURL(cd.DataVolumeImportUrlForContainerDisk(cd.ContainerDiskAlpine))),
							),
						)
						sourceVM = libstorage.RenderVMWithDataVolumeTemplate(dv)
						sourceVM.Spec.Template.Spec.Domain.Resources = virtv1.ResourceRequirements{}
						sourceVM.Spec.Instancetype = &virtv1.InstancetypeMatcher{
							Name: instancetype.Name,
							Kind: "VirtualMachineInstanceType",
						}
						sourceVM.Spec.Preference = &virtv1.PreferenceMatcher{
							Name: preference.Name,
							Kind: "VirtualMachinePreference",
						}
					})

					DescribeTable("should create new ControllerRevisions for cloned VM", Label("instancetype", "clone"), func(runStrategy virtv1.VirtualMachineRunStrategy) {
						sourceVM.Spec.RunStrategy = &runStrategy
						sourceVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Create(context.Background(), sourceVM, v1.CreateOptions{})
						Expect(err).ToNot(HaveOccurred())

						for _, dvt := range sourceVM.Spec.DataVolumeTemplates {
							libstorage.EventuallyDVWith(sourceVM.Namespace, dvt.Name, 180, HaveSucceeded())
						}
						By("Waiting until the source VM has instancetype and preference RevisionNames")
						libinstancetype.WaitForVMInstanceTypeRevisionNames(sourceVM.Name, virtClient)

						vmClone = generateCloneFromVM()
						createCloneAndWaitForFinish(vmClone)

						By("Waiting until the targetVM has instancetype and preference RevisionNames")
						libinstancetype.WaitForVMInstanceTypeRevisionNames(targetVMName, virtClient)

						By("Asserting that the targetVM has new instancetype and preference controllerRevisions")
						sourceVM, err := virtClient.VirtualMachine(testsuite.GetTestNamespace(sourceVM)).Get(context.Background(), sourceVM.Name, v1.GetOptions{})
						Expect(err).ToNot(HaveOccurred())
						targetVM, err := virtClient.VirtualMachine(testsuite.GetTestNamespace(sourceVM)).Get(context.Background(), targetVMName, v1.GetOptions{})
						Expect(err).ToNot(HaveOccurred())

						Expect(targetVM.Spec.Instancetype.RevisionName).ToNot(Equal(sourceVM.Spec.Instancetype.RevisionName), "source and target instancetype revision names should not be equal")
						Expect(targetVM.Spec.Preference.RevisionName).ToNot(Equal(sourceVM.Spec.Preference.RevisionName), "source and target preference revision names should not be equal")

						By("Asserting that the source and target ControllerRevisions contain the same Object")
						Expect(libinstancetype.EnsureControllerRevisionObjectsEqual(sourceVM.Spec.Instancetype.RevisionName, targetVM.Spec.Instancetype.RevisionName, virtClient)).To(BeTrue(), "source and target instance type controller revisions are expected to be equal")
						Expect(libinstancetype.EnsureControllerRevisionObjectsEqual(sourceVM.Spec.Preference.RevisionName, targetVM.Spec.Preference.RevisionName, virtClient)).To(BeTrue(), "source and target preference controller revisions are expected to be equal")
					},
						Entry("with a running VM", virtv1.RunStrategyAlways),
						Entry("with a stopped VM", virtv1.RunStrategyHalted),
					)
				})

				It("double cloning: clone target as a clone source", func() {
					addCloneAnnotationAndLabelFilters := func(vmClone *clone.VirtualMachineClone) {
						filters := []string{"somekey/*"}
						vmClone.Spec.LabelFilters = filters
						vmClone.Spec.AnnotationFilters = filters
						vmClone.Spec.Template.LabelFilters = filters
						vmClone.Spec.Template.AnnotationFilters = filters
					}
					generateCloneWithFilters := func(sourceVM *virtv1.VirtualMachine, targetVMName string) *clone.VirtualMachineClone {
						vmclone := generateCloneFromVMWithParams(sourceVM, targetVMName)
						addCloneAnnotationAndLabelFilters(vmclone)
						return vmclone
					}

					runStrategy := virtv1.RunStrategyHalted
					wffcSC := libstorage.IsStorageClassBindingModeWaitForFirstConsumer(snapshotStorageClass)
					if wffcSC {
						// with wffc need to start the virtual machine
						// in order for the pvc to be populated
						runStrategy = virtv1.RunStrategyAlways
					}
					sourceVM = createVMWithStorageClass(snapshotStorageClass, runStrategy)
					vmClone = generateCloneWithFilters(sourceVM, targetVMName)

					createCloneAndWaitForFinish(vmClone)

					By(fmt.Sprintf("Getting the target VM %s", targetVMName))
					targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(context.Background(), targetVMName, v1.GetOptions{})
					Expect(err).ShouldNot(HaveOccurred())
					if wffcSC {
						// run the virtual machine for the clone dv to be populated
						expectVMRunnable(targetVM)
					}

					By("Creating another clone from the target VM")
					const cloneFromCloneName = "vm-clone-from-clone"
					vmCloneFromClone := generateCloneWithFilters(targetVM, cloneFromCloneName)
					vmCloneFromClone.Name = "test-clone-from-clone"
					createCloneAndWaitForFinish(vmCloneFromClone)

					By(fmt.Sprintf("Getting the target VM %s from clone", cloneFromCloneName))
					targetVMCloneFromClone, err := virtClient.VirtualMachine(sourceVM.Namespace).Get(context.Background(), cloneFromCloneName, v1.GetOptions{})
					Expect(err).ShouldNot(HaveOccurred())

					expectVMRunnable(targetVMCloneFromClone)
					expectEqualLabels(targetVMCloneFromClone, sourceVM)
					expectEqualAnnotations(targetVMCloneFromClone, sourceVM)
					expectEqualTemplateLabels(targetVMCloneFromClone, sourceVM, "name")
					expectEqualTemplateAnnotations(targetVMCloneFromClone, sourceVM)
				})

				Context("with WaitForFirstConsumer binding mode", func() {
					BeforeEach(func() {
						snapshotStorageClass, err = libstorage.GetWFFCStorageSnapshotClass(virtClient)
						Expect(err).ToNot(HaveOccurred())
						Expect(snapshotStorageClass).ToNot(BeEmpty(), "no storage class with snapshot support and wffc binding mode")
					})

					It("should not delete the vmsnapshot and vmrestore until all the pvc(s) are bound", func() {
						addCloneAnnotationAndLabelFilters := func(vmClone *clone.VirtualMachineClone) {
							filters := []string{"somekey/*"}
							vmClone.Spec.LabelFilters = filters
							vmClone.Spec.AnnotationFilters = filters
							vmClone.Spec.Template.LabelFilters = filters
							vmClone.Spec.Template.AnnotationFilters = filters
						}
						generateCloneWithFilters := func(sourceVM *virtv1.VirtualMachine, targetVMName string) *clone.VirtualMachineClone {
							vmclone := generateCloneFromVMWithParams(sourceVM, targetVMName)
							addCloneAnnotationAndLabelFilters(vmclone)
							return vmclone
						}

						sourceVM = createVMWithStorageClass(snapshotStorageClass, virtv1.RunStrategyAlways)
						vmClone = generateCloneWithFilters(sourceVM, targetVMName)
						sourceVM, err = stopCloneVM(virtClient, sourceVM)
						Expect(err).ShouldNot(HaveOccurred())
						Eventually(ThisVMIWith(sourceVM.Namespace, sourceVM.Name), 300*time.Second, 1*time.Second).ShouldNot(Exist())
						Eventually(ThisVM(sourceVM), 300*time.Second, 1*time.Second).Should(Not(BeReady()))

						createCloneAndWaitForFinish(vmClone)

						By(fmt.Sprintf("Getting the target VM %s", targetVMName))
						targetVM, err = virtClient.VirtualMachine(sourceVM.Namespace).Get(context.Background(), targetVMName, v1.GetOptions{})
						Expect(err).ShouldNot(HaveOccurred())

						vmClone, err = virtClient.VirtualMachineClone(vmClone.Namespace).Get(context.Background(), vmClone.Name, v1.GetOptions{})
						Expect(err).ShouldNot(HaveOccurred())
						Expect(vmClone.Status.SnapshotName).ShouldNot(BeNil())
						vmSnapshotName := vmClone.Status.SnapshotName
						Expect(vmClone.Status.RestoreName).ShouldNot(BeNil())
						vmRestoreName := vmClone.Status.RestoreName
						Consistently(func(g Gomega) {
							vmSnapshot, err := virtClient.VirtualMachineSnapshot(vmClone.Namespace).Get(context.Background(), *vmSnapshotName, v1.GetOptions{})
							g.Expect(err).ShouldNot(HaveOccurred())
							g.Expect(vmSnapshot).ShouldNot(BeNil())
							vmRestore, err := virtClient.VirtualMachineRestore(vmClone.Namespace).Get(context.Background(), *vmRestoreName, v1.GetOptions{})
							g.Expect(err).ShouldNot(HaveOccurred())
							g.Expect(vmRestore).ShouldNot(BeNil())
						}, 30*time.Second).Should(Succeed(), "vmsnapshot and vmrestore should not be deleted until the pvc is bound")

						By(fmt.Sprintf("Starting the target VM %s", targetVMName))
						err = virtClient.VirtualMachine(testsuite.GetTestNamespace(targetVM)).Start(context.Background(), targetVMName, &virtv1.StartOptions{Paused: false})
						Expect(err).ToNot(HaveOccurred())
						Eventually(func(g Gomega) {
							_, err := virtClient.VirtualMachineSnapshot(vmClone.Namespace).Get(context.Background(), *vmSnapshotName, v1.GetOptions{})
							g.Expect(err).To(MatchError(errors.IsNotFound, "k8serrors.IsNotFound"))
							_, err = virtClient.VirtualMachineRestore(vmClone.Namespace).Get(context.Background(), *vmRestoreName, v1.GetOptions{})
							g.Expect(err).To(MatchError(errors.IsNotFound, "k8serrors.IsNotFound"))
						}, 1*time.Minute).Should(Succeed(), "vmsnapshot and vmrestore should be deleted once the pvc is bound")
					})

				})

			})
		})
	})
})

func startCloneVM(virtClient kubecli.KubevirtClient, vm *virtv1.VirtualMachine) (*virtv1.VirtualMachine, error) {
	patch, err := patch.New(patch.WithAdd("/spec/runStrategy", virtv1.RunStrategyAlways)).GeneratePayload()
	if err != nil {
		return nil, err
	}

	return virtClient.VirtualMachine(vm.Namespace).Patch(context.Background(), vm.Name, types.JSONPatchType, patch, v1.PatchOptions{})
}

func stopCloneVM(virtClient kubecli.KubevirtClient, vm *virtv1.VirtualMachine) (*virtv1.VirtualMachine, error) {
	patch, err := patch.New(patch.WithAdd("/spec/runStrategy", virtv1.RunStrategyHalted)).GeneratePayload()
	if err != nil {
		return nil, err
	}

	return virtClient.VirtualMachine(vm.Namespace).Patch(context.Background(), vm.Name, types.JSONPatchType, patch, v1.PatchOptions{})
}

func withFirmware(firmware *virtv1.Firmware) libvmi.Option {
	return func(vmi *virtv1.VirtualMachineInstance) {
		vmi.Spec.Domain.Firmware = firmware
	}
}

func createSourceVM(options ...libvmi.Option) (*virtv1.VirtualMachine, error) {
	vmi := libvmifact.NewCirros(options...)
	vmi.Namespace = testsuite.GetTestNamespace(nil)
	vm := libvmi.NewVirtualMachine(vmi,
		libvmi.WithAnnotations(vmi.Annotations),
		libvmi.WithLabels(vmi.Labels))

	By(fmt.Sprintf("Creating VM %s", vm.Name))
	virtClient := kubevirt.Client()
	return virtClient.VirtualMachine(vm.Namespace).Create(context.Background(), vm, v1.CreateOptions{})
}
