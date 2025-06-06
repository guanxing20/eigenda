package integration_test

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Layr-Labs/eigenda/api/clients"
	"github.com/Layr-Labs/eigenda/api/clients/v2/validator"
	clientsv2 "github.com/Layr-Labs/eigenda/api/clients/v2/validator"
	"github.com/Layr-Labs/eigenda/common"
	"github.com/Layr-Labs/eigenda/common/geth"
	verifierv2bindings "github.com/Layr-Labs/eigenda/contracts/bindings/EigenDACertVerifier"
	routerbindings "github.com/Layr-Labs/eigenda/contracts/bindings/EigenDACertVerifierRouter"
	verifierv1bindings "github.com/Layr-Labs/eigenda/contracts/bindings/EigenDACertVerifierV1"
	verifierv2legacybindings "github.com/Layr-Labs/eigenda/contracts/bindings/EigenDACertVerifierV2"

	"github.com/Layr-Labs/eigenda/core"
	"github.com/Layr-Labs/eigenda/core/eth"
	"github.com/Layr-Labs/eigenda/encoding/kzg"
	"github.com/Layr-Labs/eigenda/encoding/kzg/verifier"
	"github.com/Layr-Labs/eigenda/inabox/deploy"
	"github.com/Layr-Labs/eigensdk-go/logging"
	gcommon "github.com/ethereum/go-ethereum/common"
	ethrpc "github.com/ethereum/go-ethereum/rpc"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/ory/dockertest/v3"
)

var (
	templateName      string
	testName          string
	inMemoryBlobStore bool

	testConfig         *deploy.Config
	dockertestPool     *dockertest.Pool
	dockertestResource *dockertest.Resource
	localStackPort     string

	metadataTableName           = "test-BlobMetadata"
	bucketTableName             = "test-BucketStore"
	metadataTableNameV2         = "test-BlobMetadata-v2"
	logger                      logging.Logger
	ethClient                   common.EthClient
	rpcClient                   common.RPCEthClient
	eigenDACertVerifierV1       *verifierv1bindings.ContractEigenDACertVerifierV1
	eigenDACertVerifierV2Legacy *verifierv2legacybindings.ContractEigenDACertVerifierV2
	eigenDACertVerifier         *verifierv2bindings.ContractEigenDACertVerifier
	eigenDACertVerifierRouter   *routerbindings.ContractEigenDACertVerifierRouter
	retrievalClient             clients.RetrievalClient
	retrievalClientV2           clientsv2.ValidatorClient
	numConfirmations            int = 3
	numRetries                      = 0
	chainReader                 core.Reader

	cancel context.CancelFunc
)

func init() {
	flag.StringVar(&templateName, "config", "testconfig-anvil-nograph.yaml", "Name of the config file (in `inabox/templates`)")
	flag.StringVar(&testName, "testname", "", "Name of the test (in `inabox/testdata`)")
	flag.BoolVar(&inMemoryBlobStore, "inMemoryBlobStore", false, "whether to use in-memory blob store")
}

func TestInaboxIntegration(t *testing.T) {
	RegisterFailHandler(Fail)

	if testing.Short() {
		t.Skip()
	}

	RunSpecs(t, "Integration Suite")
}

var _ = BeforeSuite(func() {
	By("bootstrapping test environment")

	rootPath := "../../"

	var err error
	if testName == "" {
		testName, err = deploy.CreateNewTestDirectory(templateName, rootPath)
		if err != nil {
			Expect(err).To(BeNil())
		}
	}

	testConfig = deploy.NewTestConfig(testName, rootPath)
	if testConfig.Environment.IsLocal() {
		if !inMemoryBlobStore {
			fmt.Println("Using shared Blob Store")
			localStackPort = "4570"
			pool, resource, err := deploy.StartDockertestWithLocalstackContainer(localStackPort)
			Expect(err).To(BeNil())
			dockertestPool = pool
			dockertestResource = resource

			err = deploy.DeployResources(pool, localStackPort, metadataTableName, bucketTableName, metadataTableNameV2)
			Expect(err).To(BeNil())
		} else {
			fmt.Println("Using in-memory Blob Store")
		}

		fmt.Println("Starting anvil")
		testConfig.StartAnvil()

		if deployer, ok := testConfig.GetDeployer(testConfig.EigenDA.Deployer); ok && deployer.DeploySubgraphs {
			fmt.Println("Starting graph node")
			testConfig.StartGraphNode()
		}

		loggerConfig := common.DefaultLoggerConfig()
		logger, err = common.NewLogger(loggerConfig)
		Expect(err).To(BeNil())

		fmt.Println("Deploying experiment")
		testConfig.DeployExperiment()

		pk := testConfig.Pks.EcdsaMap["default"].PrivateKey
		pk = strings.TrimPrefix(pk, "0x")
		pk = strings.TrimPrefix(pk, "0X")
		ethClient, err = geth.NewMultiHomingClient(geth.EthClientConfig{
			RPCURLs:          []string{testConfig.Deployers[0].RPC},
			PrivateKeyString: pk,
			NumConfirmations: numConfirmations,
			NumRetries:       numRetries,
		}, gcommon.Address{}, logger)
		Expect(err).To(BeNil())

		rpcClient, err = ethrpc.Dial(testConfig.Deployers[0].RPC)
		Expect(err).To(BeNil())

		fmt.Println("Registering blob versions and relays")
		testConfig.RegisterBlobVersionAndRelays(ethClient)

		fmt.Println("Registering disperser keypair")
		err = testConfig.RegisterDisperserKeypair(ethClient)
		if err != nil {
			panic(err)
		}

		fmt.Println("Starting binaries")
		testConfig.StartBinaries()

		eigenDACertVerifierV1, err = verifierv1bindings.NewContractEigenDACertVerifierV1(gcommon.HexToAddress(testConfig.EigenDAV1CertVerifier), ethClient)
		Expect(err).To(BeNil())
		eigenDACertVerifierV2Legacy, err = verifierv2legacybindings.NewContractEigenDACertVerifierV2(gcommon.HexToAddress(testConfig.EigenDA.CertVerifierLegacy), ethClient)
		Expect(err).To(BeNil())
		eigenDACertVerifier, err = verifierv2bindings.NewContractEigenDACertVerifier(gcommon.HexToAddress(testConfig.EigenDA.CertVerifier), ethClient)
		Expect(err).To(BeNil())
		eigenDACertVerifierRouter, err = routerbindings.NewContractEigenDACertVerifierRouter(gcommon.HexToAddress(testConfig.EigenDA.CertVerifierRouter), ethClient)
		Expect(err).To(BeNil())
		err = setupRetrievalClient(testConfig)
		Expect(err).To(BeNil())
	}
})

func setupRetrievalClient(testConfig *deploy.Config) error {
	ethClientConfig := geth.EthClientConfig{
		RPCURLs:          []string{testConfig.Deployers[0].RPC},
		PrivateKeyString: "351b8eca372e64f64d514f90f223c5c4f86a04ff3dcead5c27293c547daab4ca", // just random private key
		NumConfirmations: numConfirmations,
		NumRetries:       numRetries,
	}
	var err error
	if ethClient == nil {
		ethClient, err = geth.NewMultiHomingClient(ethClientConfig, gcommon.Address{}, logger)
		if err != nil {
			return err
		}
	}
	if rpcClient == nil {
		rpcClient, err = ethrpc.Dial(testConfig.Deployers[0].RPC)
		if err != nil {
			log.Fatalln("could not start tcp listener", err)
		}
	}
	tx, err := eth.NewWriter(logger, ethClient, testConfig.Retriever.RETRIEVER_BLS_OPERATOR_STATE_RETRIVER, testConfig.Retriever.RETRIEVER_EIGENDA_SERVICE_MANAGER)
	if err != nil {
		return err
	}

	cs := eth.NewChainState(tx, ethClient)
	agn := &core.StdAssignmentCoordinator{}
	nodeClient := clients.NewNodeClient(20 * time.Second)
	srsOrder, err := strconv.Atoi(testConfig.Retriever.RETRIEVER_SRS_ORDER)
	if err != nil {
		return err
	}
	kzgConfig := &kzg.KzgConfig{
		G1Path:          testConfig.Retriever.RETRIEVER_G1_PATH,
		G2Path:          testConfig.Retriever.RETRIEVER_G2_PATH,
		G2PowerOf2Path:  testConfig.Retriever.RETRIEVER_G2_POWER_OF_2_PATH,
		CacheDir:        testConfig.Retriever.RETRIEVER_CACHE_PATH,
		SRSOrder:        uint64(srsOrder),
		SRSNumberToLoad: uint64(srsOrder),
		NumWorker:       1,
		PreloadEncoder:  false,
		LoadG2Points:    true,
	}

	v, err := verifier.NewVerifier(kzgConfig, nil)
	if err != nil {
		return err
	}

	retrievalClient, err = clients.NewRetrievalClient(logger, cs, agn, nodeClient, v, 10)
	if err != nil {
		return err
	}
	chainReader, err = eth.NewReader(
		logger,
		ethClient,
		testConfig.Retriever.RETRIEVER_BLS_OPERATOR_STATE_RETRIVER,
		testConfig.Retriever.RETRIEVER_EIGENDA_SERVICE_MANAGER)
	if err != nil {
		return err
	}
	clientConfig := validator.DefaultClientConfig()
	retrievalClientV2 = clientsv2.NewValidatorClient(logger, chainReader, cs, v, clientConfig, nil)

	return nil
}

var _ = AfterSuite(func() {
	if testConfig.Environment.IsLocal() {
		if cancel != nil {
			cancel()
		}

		fmt.Println("Stopping binaries")
		testConfig.StopBinaries()

		fmt.Println("Stopping anvil")
		testConfig.StopAnvil()

		fmt.Println("Stopping graph node")
		testConfig.StopGraphNode()

		deploy.PurgeDockertestResources(dockertestPool, dockertestResource)
	}
})
