/*
Copyright The CloudNativePG Contributors

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

package e2e

import (
	"fmt"
	"strings"

	"github.com/cloudnative-pg/cloudnative-pg/internal/cmd/manager/walrestore"
	"github.com/cloudnative-pg/cloudnative-pg/pkg/specs"
	"github.com/cloudnative-pg/cloudnative-pg/tests"
	testUtils "github.com/cloudnative-pg/cloudnative-pg/tests/utils"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// This e2e test is to test the wal-restore handling when maxParallel (specified as "3" in this testing) is specified in
// wal section under backup for wal archive storing/recovering. To facilitate controlling the testing, we directly forge
// wals on the object storage ("minio" in this testing) by copying and renaming an existing wal file.

var _ = Describe("Wal-restore in parallel", Label(tests.LabelBackupRestore), func() {
	const (
		level             = tests.High
		walRestoreCommand = "/controller/manager wal-restore"
		PgWalPath         = specs.PgWalPath
		SpoolDirectory    = walrestore.SpoolDirectory
	)

	var namespace, clusterName string
	var primary, standby, latestWAL, walFile1, walFile2, walFile3, walFile4, walFile5, walFile6 string

	BeforeEach(func() {
		if testLevelEnv.Depth < int(level) {
			Skip("Test depth is lower than the amount requested for this test")
		}
		if env.IsIBM() {
			Skip("This test is not run on an IBM architecture")
		}
	})

	JustAfterEach(func() {
		if CurrentSpecReport().Failed() {
			env.DumpClusterEnv(namespace, clusterName,
				"out/"+CurrentSpecReport().LeafNodeText+".log")
		}
	})

	AfterEach(func() {
		err := env.DeleteNamespace(namespace)
		Expect(err).ToNot(HaveOccurred())
	})

	It("Wal-restore in parallel using minio as object storage for backup", func() {
		// This is a set of tests using a minio server deployed in the same
		// namespace as the cluster. Since each cluster is installed in its
		// own namespace, they can share the configuration file

		const (
			clusterWithMinioSampleFile = fixturesDir + "/backup/minio/cluster-with-backup-minio-with-wal-max-parallel.yaml"
		)

		namespace = "pg-backup-minio-wal-max-parallel"
		clusterName, err := env.GetResourceNameFromYAML(clusterWithMinioSampleFile)
		Expect(err).ToNot(HaveOccurred())

		err = env.CreateNamespace(namespace)
		Expect(err).ToNot(HaveOccurred())

		By("creating the credentials for minio", func() {
			AssertStorageCredentialsAreCreated(namespace, "backup-storage-creds", "minio", "minio123")
		})

		By("setting up minio", func() {
			setup, err := testUtils.MinioDefaultSetup(namespace)
			Expect(err).ToNot(HaveOccurred())
			err = testUtils.InstallMinio(env, setup, 300)
			Expect(err).ToNot(HaveOccurred())
		})

		// Create the minio client pod and wait for it to be ready.
		// We'll use it to check if everything is archived correctly
		By("setting up minio client pod", func() {
			minioClient := testUtils.MinioDefaultClient(namespace)
			err := testUtils.PodCreateAndWaitForReady(env, &minioClient, 240)
			Expect(err).ToNot(HaveOccurred())
		})

		// Create the cluster and assert it be ready
		AssertCreateCluster(namespace, clusterName, clusterWithMinioSampleFile, env)

		// Get the primary
		pod, err := env.GetClusterPrimary(namespace, clusterName)
		Expect(err).ToNot(HaveOccurred())
		primary = pod.GetName()

		// Get the standby
		podList, err := env.GetClusterPodList(namespace, clusterName)
		for _, po := range podList.Items {
			if po.Name != primary {
				// Only one standby in this specific testing
				standby = po.GetName()
				break
			}
		}
		Expect(err).ToNot(HaveOccurred())

		// Make sure both Wal-archive and Minio work
		// Create a WAL on the primary and check if it arrives at minio, within a short time
		By("archiving WALs and verifying they exist", func() {
			pod, err := env.GetClusterPrimary(namespace, clusterName)
			Expect(err).ToNot(HaveOccurred())
			primary := pod.GetName()
			out, _, err := testUtils.Run(fmt.Sprintf(
				"kubectl exec -n %v %v -- %v",
				namespace,
				primary,
				switchWalCmd))
			Expect(err).ToNot(HaveOccurred())

			latestWAL = strings.TrimSpace(out)
			Eventually(func() (int, error) {
				// WALs are compressed with gzip in the fixture
				return testUtils.CountFilesOnMinio(namespace, minioClientName, latestWAL+".gz")
			}, 30).Should(BeEquivalentTo(1))
		})

		By("forging 5 wals on Minio by copying and renaming an existing archive file", func() {
			walFile1 = "0000000100000000000000F1"
			walFile2 = "0000000100000000000000F2"
			walFile3 = "0000000100000000000000F3"
			walFile4 = "0000000100000000000000F4"
			walFile5 = "0000000100000000000000F5"
			Expect(testUtils.ForgeArchiveWalOnMinio(namespace, clusterName, minioClientName, latestWAL, walFile1)).
				ShouldNot(HaveOccurred())
			Expect(testUtils.ForgeArchiveWalOnMinio(namespace, clusterName, minioClientName, latestWAL, walFile2)).
				ShouldNot(HaveOccurred())
			Expect(testUtils.ForgeArchiveWalOnMinio(namespace, clusterName, minioClientName, latestWAL, walFile3)).
				ShouldNot(HaveOccurred())
			Expect(testUtils.ForgeArchiveWalOnMinio(namespace, clusterName, minioClientName, latestWAL, walFile4)).
				ShouldNot(HaveOccurred())
			Expect(testUtils.ForgeArchiveWalOnMinio(namespace, clusterName, minioClientName, latestWAL, walFile5)).
				ShouldNot(HaveOccurred())
		})

		By("asserting the spool directory is empty on the standby", func() {
			if !testUtils.TestDirectoryEmpty(namespace, standby, SpoolDirectory) {
				purgeSpoolDirectoryCmd := "rm " + SpoolDirectory + "/*"
				_, _, err := testUtils.Run(fmt.Sprintf(
					"kubectl exec -n %v %v -- %v",
					namespace,
					standby,
					purgeSpoolDirectoryCmd))
				Expect(err).ShouldNot(HaveOccurred())
			}
		})

		// Invoke the wal-restore command through exec requesting the #1 file.
		// Expected outcome:
		// 		exit code 0, #1 is in the output location, #2 and #3 are in the spool directory.
		// 		The flag is unset.
		By("invoking the wal-restore command requesting #1 wal", func() {
			_, _, err := testUtils.Run(fmt.Sprintf(
				"kubectl exec -n %v %v -- %v",
				namespace,
				standby,
				walRestoreCommand+" "+walFile1+" "+PgWalPath+"/"+walFile1))
			Expect(err).ToNot(HaveOccurred(), "exit code should be 0")
			Expect(testUtils.TestFileExist(namespace, standby, PgWalPath, walFile1)).To(Equal(true),
				"#1 wal is in the output location")
			Expect(testUtils.TestFileExist(namespace, standby, SpoolDirectory, walFile2)).To(Equal(true),
				"#2 wal is in the spool directory")
			Expect(testUtils.TestFileExist(namespace, standby, SpoolDirectory, walFile3)).To(Equal(true),
				"#3 wal is in the spool directory")
			Expect(testUtils.TestFileExist(namespace, standby, SpoolDirectory, "end-of-wal-stream")).
				To(Equal(false), "end-of-wal-stream flag is unset")
		})

		// Invoke the wal-restore command through exec requesting the #2 file.
		// Expected outcome:
		// 		exit code 0, #2 is in the output location, #3 is in the spool directory.
		// 		The flag is unset.
		By("invoking the wal-restore command requesting #2 wal", func() {
			_, _, err := testUtils.Run(fmt.Sprintf(
				"kubectl exec -n %v %v -- %v",
				namespace,
				standby,
				walRestoreCommand+" "+walFile2+" "+PgWalPath+"/"+walFile2))
			Expect(err).ToNot(HaveOccurred(), "exit code should be 0")
			Expect(testUtils.TestFileExist(namespace, standby, PgWalPath, walFile2)).To(Equal(true),
				"#2 wal is in the output location")
			Expect(testUtils.TestFileExist(namespace, standby, SpoolDirectory, walFile3)).To(Equal(true),
				"#3 wal is in the spool directory")
			Expect(testUtils.TestFileExist(namespace, standby, SpoolDirectory, "end-of-wal-stream")).
				To(Equal(false), "end-of-wal-stream flag is unset")
		})

		// Invoke the wal-restore command through exec requesting the #3 file.
		// Expected outcome:
		// 		exit code 0, #3 is in the output location, spool directory is empty.
		// 		The flag is unset.
		By("invoking the wal-restore command requesting #3 wal", func() {
			_, _, err := testUtils.Run(fmt.Sprintf(
				"kubectl exec -n %v %v -- %v",
				namespace,
				standby,
				walRestoreCommand+" "+walFile3+" "+PgWalPath+"/"+walFile3))
			Expect(err).ToNot(HaveOccurred(), "exit code should be 0")
			Expect(testUtils.TestFileExist(namespace, standby, PgWalPath, walFile3)).To(Equal(true),
				"#3 wal is in the output location")
			Expect(testUtils.TestDirectoryEmpty(namespace, standby, SpoolDirectory)).To(Equal(true),
				"spool directory is empty, end-of-wal-stream flag is unset")
		})

		// Invoke the wal-restore command through exec requesting the #4 file.
		// Expected outcome:
		// 		exit code 0, #4 is in the output location, #5 is in the spool directory.
		// 		The flag is set because #6 file not present.
		By("invoking the wal-restore command requesting #4 wal", func() {
			_, _, err := testUtils.Run(fmt.Sprintf(
				"kubectl exec -n %v %v -- %v",
				namespace,
				standby,
				walRestoreCommand+" "+walFile4+" "+PgWalPath+"/"+walFile4))
			Expect(err).ToNot(HaveOccurred())
			Expect(testUtils.TestFileExist(namespace, standby, PgWalPath, walFile4)).To(Equal(true),
				"#4 wal is in the output location")
			Expect(testUtils.TestFileExist(namespace, standby, SpoolDirectory, walFile5)).To(Equal(true),
				"#5 wal is in the spool directory")
			Expect(testUtils.TestFileExist(namespace, standby, SpoolDirectory, "end-of-wal-stream")).
				To(Equal(true), "end-of-wal-stream flag is set for #6 wal is not present")
		})

		// Generate a new wal file; the archive also contains WAL #6.
		By("forging a new wal file, the #6 wal", func() {
			walFile6 = "0000000100000000000000F6"
			Expect(testUtils.ForgeArchiveWalOnMinio(namespace, clusterName, minioClientName, latestWAL, walFile6)).
				ShouldNot(HaveOccurred())
		})

		// Invoke the wal-restore command through exec requesting the #5 file.
		// Expected outcome:
		//		exit code 0, #5 is in the output location, no files in the spool directory. The flag is still present.
		By("invoking the wal-restore command requesting #5 wal", func() {
			_, _, err := testUtils.Run(fmt.Sprintf(
				"kubectl exec -n %v %v -- %v",
				namespace,
				standby,
				walRestoreCommand+" "+walFile5+" "+PgWalPath+"/"+walFile5))
			Expect(err).ToNot(HaveOccurred(), "exit code should be 0")
			Expect(testUtils.TestFileExist(namespace, standby, PgWalPath, walFile5)).To(Equal(true),
				"#5 wal is in the output location")
			Expect(testUtils.TestFileExist(namespace, standby, SpoolDirectory, "00000001*")).To(Equal(false),
				"no wal files exist in the spool directory")
			Expect(testUtils.TestFileExist(namespace, standby, SpoolDirectory, "end-of-wal-stream")).
				To(Equal(true), "end-of-wal-stream flag is still there")
		})

		// Invoke the wal-restore command through exec requesting the #6 file.
		// Expected outcome:
		//		exit code 1, output location untouched, no files in the spool directory. The flag is unset.
		By("invoking the wal-restore command requesting #6 wal", func() {
			_, _, err := testUtils.RunUnchecked(fmt.Sprintf(
				"kubectl exec -n %v %v -- %v",
				namespace,
				standby,
				walRestoreCommand+" "+walFile6+" "+PgWalPath+"/"+walFile6))
			Expect(err).To(HaveOccurred(),
				"exit code should 1 since #6 wal is not in the output location or spool directory and flag is set")
			Expect(testUtils.TestFileExist(namespace, standby, PgWalPath, walFile6)).ToNot(Equal(true),
				"#6 wal is not in the output location")
			Expect(testUtils.TestDirectoryEmpty(namespace, standby, SpoolDirectory)).To(Equal(true),
				"spool directory is empty, end-of-wal-stream flag is unset")
		})

		// Invoke the wal-restore command through exec requesting the #6 file again.
		// Expected outcome:
		//		exit code 0, #6 is in the output location, no files in the spool directory.
		//		The flag is present again because #7 and #8 are unavailable.
		By("invoking the wal-restore command requesting #6 wal again", func() {
			_, _, err := testUtils.Run(fmt.Sprintf(
				"kubectl exec -n %v %v -- %v",
				namespace,
				standby,
				walRestoreCommand+" "+walFile6+" "+PgWalPath+"/"+walFile6))
			Expect(err).ToNot(HaveOccurred(), "exit code should be 0")
			Expect(testUtils.TestFileExist(namespace, standby, PgWalPath, walFile6)).To(Equal(true),
				"#6 wal is in the output location")
			Expect(testUtils.TestFileExist(namespace, standby, SpoolDirectory, "00000001*")).ToNot(Equal(true),
				"no wals in the spool directory")
			Expect(testUtils.TestFileExist(namespace, standby, SpoolDirectory, "end-of-wal-stream")).
				To(Equal(true), "end-of-wal-stream flag is set for #7 and #8 wal is not present")
		})
	})
})
