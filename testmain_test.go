package gocb

import (
	"flag"
	"fmt"
	gojcbmock "github.com/couchbase/gocbcore/v9/jcbmock"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"
)

var globalConfig testConfig

var globalBucket *Bucket
var globalCollection *Collection
var globalScope *Scope
var globalCluster *testCluster
var globalTracer *testTracer
var globalMeter *testMeter

type testConfig struct {
	Server       string
	User         string
	Password     string
	Bucket       string
	Version      string
	Collection   string
	Scope        string
	FeatureFlags []TestFeatureFlag

	connstr string
	auth    Authenticator
}

func TestMain(m *testing.M) {
	initialGoroutineCount := runtime.NumGoroutine()

	server := envFlagString("GOCBSERVER", "server", "",
		"The connection string to connect to for a real server")
	user := envFlagString("GOCBUSER", "user", "",
		"The username to use to authenticate when using a real server")
	password := envFlagString("GOCBPASS", "pass", "",
		"The password to use to authenticate when using a real server")
	bucketName := envFlagString("GOCBBUCKET", "bucket", "default",
		"The bucket to use to test against")
	version := envFlagString("GOCBVER", "version", "",
		"The server version being tested against (major.minor.patch.build_edition)")
	collectionName := envFlagString("GOCBCOLL", "collection-name", "",
		"The name of the collection to use")
	scopeName := envFlagString("GOCBSCOP", "scope-name", "",
		"The name of the scope to use")
	featuresToTest := envFlagString("GOCBFEAT", "features", "",
		"The features that should be tested, applicable only for integration test runs")
	disableLogger := envFlagBool("GOCBNOLOG", "disable-logger", false,
		"Whether to disable the logger")
	flag.Parse()

	if testing.Short() {
		mustBeNil := func(val interface{}, name string) {
			flag.Visit(func(f *flag.Flag) {
				if f.Name == name {
					panic(name + " cannot be used in short mode")
				}
			})
		}
		mustBeNil(server, "server")
		mustBeNil(user, "user")
		mustBeNil(password, "pass")
		mustBeNil(bucketName, "bucket")
		mustBeNil(version, "version")
		mustBeNil(collectionName, "collection-name")
		mustBeNil(scopeName, "scope-name")
	}

	var featureFlags []TestFeatureFlag
	featureFlagStrs := strings.Split(*featuresToTest, ",")
	for _, featureFlagStr := range featureFlagStrs {
		if len(featureFlagStr) == 0 {
			continue
		}

		if featureFlagStr[0] == '+' {
			featureFlags = append(featureFlags, TestFeatureFlag{
				Enabled: true,
				Feature: FeatureCode(featureFlagStr[1:]),
			})
			continue
		} else if featureFlagStr[0] == '-' {
			featureFlags = append(featureFlags, TestFeatureFlag{
				Enabled: false,
				Feature: FeatureCode(featureFlagStr[1:]),
			})
			continue
		}

		panic("failed to parse specified feature codes")
	}

	if !*disableLogger {
		SetLogger(VerboseStdioLogger())
	}

	globalConfig.Server = *server
	globalConfig.User = *user
	globalConfig.Password = *password
	globalConfig.Bucket = *bucketName
	globalConfig.Version = *version
	globalConfig.Collection = *collectionName
	globalConfig.Scope = *scopeName
	globalConfig.FeatureFlags = featureFlags

	if !testing.Short() {
		setupCluster()
	}

	result := m.Run()

	if globalCluster != nil {
		err := globalCluster.Close(nil)
		if err != nil {
			panic(err)
		}
	}

	// Loop for at most a second checking for goroutines leaks, this gives any HTTP goroutines time to shutdown
	start := time.Now()
	var finalGoroutineCount int
	for time.Now().Sub(start) <= 1*time.Second {
		runtime.Gosched()
		finalGoroutineCount = runtime.NumGoroutine()
		if finalGoroutineCount == initialGoroutineCount {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if finalGoroutineCount != initialGoroutineCount {
		log.Printf("Detected a goroutine leak (%d before != %d after), failing", initialGoroutineCount, finalGoroutineCount)
		pprof.Lookup("goroutine").WriteTo(os.Stdout, 1)
		result = 1
	} else {
		log.Printf("No goroutines appear to have leaked (%d before == %d after)", initialGoroutineCount, finalGoroutineCount)
	}

	os.Exit(result)
}

func envFlagString(envName, name, value, usage string) *string {
	envValue := os.Getenv(envName)
	if envValue != "" {
		value = envValue
	}
	return flag.String(name, value, usage)
}

func envFlagBool(envName, name string, value bool, usage string) *bool {
	envValue := os.Getenv(envName)
	if envValue != "" {
		if envValue == "0" {
			value = false
		} else if strings.ToLower(envValue) == "false" {
			value = false
		} else {
			value = true
		}
	}
	return flag.Bool(name, value, usage)
}

func setupCluster() {
	var err error
	var connStr string
	var mock *gojcbmock.Mock
	var auth PasswordAuthenticator
	if globalConfig.Server == "" {
		if globalConfig.Version != "" {
			panic("version cannot be specified with mock")
		}

		mpath, err := gojcbmock.GetMockPath()
		if err != nil {
			panic(err.Error())
		}

		globalConfig.Bucket = "default"
		mock, err = gojcbmock.NewMock(mpath, 4, 1, 64, []gojcbmock.BucketSpec{
			{Name: "default", Type: gojcbmock.BCouchbase},
		}...)
		if err != nil {
			panic(err.Error())
		}

		mock.Control(gojcbmock.NewCommand(gojcbmock.CSetCCCP,
			map[string]interface{}{"enabled": "true"}))
		mock.Control(gojcbmock.NewCommand(gojcbmock.CSetSASLMechanisms,
			map[string]interface{}{"mechs": []string{"SCRAM-SHA512"}}))

		globalConfig.Version = mock.Version()

		var addrs []string
		for _, mcport := range mock.MemcachedPorts() {
			addrs = append(addrs, fmt.Sprintf("127.0.0.1:%d", mcport))
		}
		connStr = fmt.Sprintf("couchbase://%s", strings.Join(addrs, ","))
		globalConfig.Server = connStr
		auth = PasswordAuthenticator{
			Username: "Administrator",
			Password: "password",
		}
	} else {
		connStr = globalConfig.Server

		auth = PasswordAuthenticator{
			Username: globalConfig.User,
			Password: globalConfig.Password,
		}

		if globalConfig.Version == "" {
			globalConfig.Version = defaultServerVersion
		}
	}

	globalTracer = newTestTracer()
	globalMeter = newTestMeter()

	cluster, err := Connect(connStr, ClusterOptions{
		Authenticator: auth,
		Tracer:        globalTracer,
		Meter:         globalMeter,
	})
	if err != nil {
		panic(err.Error())
	}

	globalConfig.connstr = connStr
	globalConfig.auth = auth

	nodeVersion, err := newNodeVersion(globalConfig.Version, mock != nil)
	if err != nil {
		panic(err.Error())
	}

	globalCluster = &testCluster{
		Cluster:      cluster,
		Mock:         mock,
		Version:      nodeVersion,
		FeatureFlags: globalConfig.FeatureFlags,
	}

	globalBucket = globalCluster.Bucket(globalConfig.Bucket)

	if globalConfig.Scope != "" {
		globalScope = globalBucket.Scope(globalConfig.Scope)
	} else {
		globalScope = globalBucket.DefaultScope()
	}

	if globalConfig.Collection != "" {
		globalCollection = globalScope.Collection(globalConfig.Collection)
	} else {
		globalCollection = globalScope.Collection("_default")
	}
}
