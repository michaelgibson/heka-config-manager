package config_management

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/bbangert/toml"
	"github.com/mozilla-services/heka/message"
	. "github.com/mozilla-services/heka/pipeline"
	"github.com/pborman/uuid"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	PROCESSINPUT_TICKER = 15
)

type CMBatch struct {
	queueCursor string
	count       int64
	batch       []byte
	confInfo    ConfigFileInfo
}

type ConfigFileInfo struct {
	confName     string
	confType     string
	confCategory string
	fileName     string
	overwrite    bool
	status       string
	ticker       int
}

type CMFilter struct {
	*CMFilterConfig
	count        int64
	backChan     chan []byte
	recvChan     chan MsgPack
	batchChan    chan CMBatch // Chan to pass completed batches
	outBatch     []byte
	queueCursor  string
	conf         *CMFilterConfig
	fr           FilterRunner
	outputBlock  *RetryHelper
	reportLock   sync.Mutex
	pConfig      *PipelineConfig
	stopChan     chan bool
	msgLoopCount uint
	msg          *message.Message
	confInfo     ConfigFileInfo
	includePaths []string
	shareDir     string
}

type CMFilterConfig struct {
	CMTag        string   `toml:"cm_tag"`
	ExcludePaths []string `toml:"exclude_paths"`
	IncludeTypes []string `toml:"include_types"`

	//Used for ProcessDirectoryInputs
	ProcessDir     string `toml:"process_dir"`
	LogstreamerDir string `toml:"logstreamer_dir"`
	HttpDir        string `toml:"http_dir"`
	FilePollingDir    string `toml:"files_dir"`

	UseBuffering bool `toml:"use_buffering"`
}

type MsgPack struct {
	bytes        []byte
	Message      *message.Message
	MsgLoopCount uint
	queueCursor  string
}

func (f *CMFilter) ConfigStruct() interface{} {
	return &CMFilterConfig{
		CMTag:        "CM",
		IncludeTypes: []string{"ProcessInput", "LogstreamerInput", "HttpInput", "FilePollingInput"},
		UseBuffering: true,
	}
}

func (f *CMFilter) Init(config interface{}) (err error) {
	f.conf = config.(*CMFilterConfig)

	f.batchChan = make(chan CMBatch)
	f.backChan = make(chan []byte, 2)
	f.recvChan = make(chan MsgPack, 100)

	return
}

func (f *CMFilter) Prepare(fr FilterRunner, h PluginHelper) error {
	f.fr = fr
	f.stopChan = fr.StopChan()
	f.confInfo.ticker = PROCESSINPUT_TICKER
	f.pConfig = h.PipelineConfig()
	f.shareDir = f.pConfig.Globals.ShareDir

	if f.conf.ProcessDir == "" {
		f.conf.ProcessDir = filepath.Join(f.shareDir, "processes.d")
	}

	if f.conf.LogstreamerDir == "" {
		f.conf.LogstreamerDir = filepath.Join(f.shareDir, "logstreamers.d")
	}

	if f.conf.HttpDir == "" {
		f.conf.HttpDir = filepath.Join(f.shareDir, "http.d")
	}

	if f.conf.FilePollingDir == "" {
		f.conf.FilePollingDir = filepath.Join(f.shareDir, "files.d")
	}

	f.includePaths = append(f.includePaths, f.conf.ProcessDir)
	f.includePaths = append(f.includePaths, f.conf.LogstreamerDir)
	f.includePaths = append(f.includePaths, f.conf.HttpDir)
	f.includePaths = append(f.includePaths, f.conf.FilePollingDir)

	var err error
	f.outputBlock, err = NewRetryHelper(RetryOptions{
		MaxDelay:   "5s",
		MaxRetries: -1,
	})
	if err != nil {
		return fmt.Errorf("can't create retry helper: %s", err.Error())
	}

	f.outBatch = make([]byte, 0, 10000)
	go f.committer(h)
	go f.batchSender(h)
	return nil

}

func (f *CMFilter) ProcessMessage(pack *PipelinePack) error {
	var (
		outBytes []byte
	)

	outBytes = []byte(pack.Message.GetPayload())

	if outBytes != nil {
		f.recvChan <- MsgPack{bytes: outBytes, queueCursor: pack.QueueCursor, Message: pack.Message, MsgLoopCount: pack.MsgLoopCount}
	}

	return nil
}

func (f *CMFilter) batchSender(h PluginHelper) {
	ok := true
	outBytes := make([]byte, 0, 10000)

	for ok {
		select {
		case <-f.stopChan:
			ok = false
			continue
		case pack := <-f.recvChan:

			f.msgLoopCount = pack.MsgLoopCount
			action, _ := pack.Message.GetFieldValue("Action")
			f.confInfo = ConfigFileInfo{
				confName:     "",
				confType:     "",
				confCategory: "",
				fileName:     "",
				overwrite:    false,
				status:       "OK",
				ticker:       PROCESSINPUT_TICKER,
			}

			switch action {
			case "add":
				payload := pack.Message.GetPayload()
				if value, exists := pack.Message.GetFieldValue("Overwrite"); exists {
					field := pack.Message.FindFirstField("Overwrite")
					valueType := field.GetValueType()
					if valueType == message.Field_STRING {
						f.confInfo.overwrite, _ = strconv.ParseBool(value.(string))
					}
				}

				tomlSection, _ := f.getDecodedToml(payload)

			CONFIG:
				for name, conf := range tomlSection {
					f.confInfo.confName, f.confInfo.confType, f.confInfo.confCategory, f.confInfo.ticker = f.getConfigFileInfo(name, conf)
					for _, confType := range f.conf.IncludeTypes {
						if f.confInfo.confType == confType {
							outBytes = []byte(f.addConfig(payload, f.confInfo))
							continue CONFIG
						}
					}
				}

			case "delete":
				if value, exists := pack.Message.GetFieldValue("Filename"); exists {
					field := pack.Message.FindFirstField("Filename")
					valueType := field.GetValueType()
					if valueType == message.Field_STRING {
						outBytes = []byte(f.removeConfig(value.(string)))
					}
				} else {
					f.confInfo.status = "ERROR"
					outBytes = []byte("You must specify a 'Filename' Field when deleting.")
				}

			case "return":
				f.queueCursor = pack.queueCursor
				for _, configPath := range f.includePaths {
					err := f.sendConfigContents(configPath)
					if err != nil {
						fmt.Println(err)
						f.pConfig.Globals.ShutDown(1)
						break
					}
				}
			default:
				outBytes = []byte("ERROR Applying Configuration: You must supply an action(add, remove, return)")
			}
			f.outBatch = append(f.outBatch, outBytes...)
			f.queueCursor = pack.queueCursor
			f.count++
			if len(f.outBatch) > 0 {
				f.sendBatch()
			}
		}

	}
}

func (f *CMFilter) sendBatch() {
	b := CMBatch{
		queueCursor: f.queueCursor,
		count:       f.count,
		batch:       f.outBatch,
		confInfo:    f.confInfo,
	}
	f.count = 0
	select {
	case <-f.stopChan:
		return
	case f.batchChan <- b:
	}
	select {
	case <-f.stopChan:
	case f.outBatch = <-f.backChan:
	}
}

func (f *CMFilter) committer(h PluginHelper) {
	f.backChan <- make([]byte, 0, 10000)

	var (
		b   CMBatch
		tag string
	)
	tag = f.conf.CMTag

	ok := true
	for ok {
		select {
		case <-f.stopChan:
			ok = false
			continue
		case b, ok = <-f.batchChan:
			if !ok {
				continue
			}
		}

		pack, _ := h.PipelinePack(f.msgLoopCount)

		fileNameField, _ := message.NewField("FileName", b.confInfo.fileName, "")
		pack.Message.AddField(fileNameField)

		confNameField, _ := message.NewField("ConfName", b.confInfo.confName, "")
		pack.Message.AddField(confNameField)

		confTypeField, _ := message.NewField("ConfType", b.confInfo.confType, "")
		pack.Message.AddField(confTypeField)

		confCategoryField, _ := message.NewField("ConfCategory", b.confInfo.confCategory, "")
		pack.Message.AddField(confCategoryField)

		if b.confInfo.confType == "ProcessInput" {
			confTickerField, _ := message.NewField("Ticker", strconv.Itoa(b.confInfo.ticker), "")
			pack.Message.AddField(confTickerField)
		}

		statusField, _ := message.NewField("Status", b.confInfo.status, "")
		pack.Message.AddField(statusField)

		tagField, _ := message.NewField("CMTag", tag, "")
		pack.Message.AddField(tagField)
		pack.Message.SetUuid(uuid.NewRandom())
		pack.Message.SetPayload(string(b.batch))

		f.fr.Inject(pack)
		f.fr.UpdateCursor(b.queueCursor)

		b.batch = b.batch[:0]
		f.backChan <- b.batch
	}
}

//BEGIN "add" Config methods
func (f *CMFilter) addConfig(payload string, cfi ConfigFileInfo) (result string) {
	var (
		buffer      bytes.Buffer
		dup         = false
		dir         string
		existingCFI ConfigFileInfo
	)

	switch cfi.confType {
	case "ProcessInput":
		dir = filepath.Clean(f.conf.ProcessDir) + "/" + fmt.Sprint(cfi.ticker)
	case "LogstreamerInput":
		dir = filepath.Clean(f.conf.LogstreamerDir)
	case "HttpInput":
		dir = filepath.Clean(f.conf.HttpDir)
	case "FilePollingInput":
		dir = filepath.Clean(f.conf.FilePollingDir)
	default:
		f.confInfo.status = "ERROR"
		return fmt.Sprintf("Unable to determine configuration directory.")
	}

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		os.Mkdir(dir, 0755)
	}

	randBytes := make([]byte, 16)
	rand.Read(randBytes)
	fName := "heka" + hex.EncodeToString(randBytes) + ".toml"
	confFile := filepath.Join(dir, fName)

	//check for duplicate entries
	files, _ := ioutil.ReadDir(dir)
	for _, file := range files {
		existingFName := filepath.Join(dir, file.Name())
		fileContents, _ := ReplaceEnvsFile(existingFName)
		fileTomlConfig, _ := f.getDecodedToml(fileContents)
		for name, conf := range fileTomlConfig {
			existingCFI.confName, existingCFI.confType, existingCFI.confCategory, existingCFI.ticker = f.getConfigFileInfo(name, conf)

			//Check if there is already a config with this name at the same ticker interval.
			if cfi.confName == existingCFI.confName && cfi.ticker == existingCFI.ticker {
				dup = true
			}
		}

		if payload == fileContents || dup {
			f.confInfo.fileName = existingFName
			if !cfi.overwrite {
				f.confInfo.status = "ERROR"
				return fmt.Sprintf("Duplicate config already exists for this interval and overwrite is disabled.\nNot adding configuration.\nFilename: %s\n%s", existingFName, fileContents)
			} else {
				confFile = existingFName
				buffer.WriteString(fmt.Sprintf("Overwriting existing configuration.\nFilename: %s\n%s\n", existingFName, fileContents))
			}
		}
	}
	err := ioutil.WriteFile(confFile, []byte(payload), 0644)

	if err != nil {
		f.confInfo.status = "ERROR"
		return fmt.Sprintf("Failed due to error: %s", err.Error())
	}
	f.confInfo.fileName = confFile
	buffer.WriteString("Successfully updated configuation!")
	return buffer.String()
}

func (f *CMFilter) getDecodedToml(contents string) (configFile ConfigFile, err error) {
	//attempt make toml primitive from string
	if _, err = toml.Decode(contents, &configFile); err != nil {
		return configFile, fmt.Errorf("Error decoding config file: %s", err)
	}
	return configFile, err
}

//END "add" Config methods

//BEGIN "remove" Config methods
func (f *CMFilter) removeConfig(fName string) (result string) {
	f.confInfo.fileName = fName
	fi, err := os.Stat(fName)

	if err != nil {
		f.confInfo.status = "ERROR"
		result = fmt.Sprintf("can't stat file: " + err.Error())
		return result
	}

	//I don't always delete directories, but when I do, I prefer that they are only empty sub-directories of the ProcessDir
	if fi.IsDir() && f.confInfo.confType == "ProcessDirectoryInput" {
		err := f.checkProcessTickerDir(fName)
		if err != nil {
			//Stay employed my friends...
			f.confInfo.status = "ERROR"
			result = fmt.Sprintf("You cannot delete this directory: %s", fName)
			return result
		}
	}

	parentDir := filepath.Dir(fName)

	_, err = f.checkPath(fName)

	if err != nil {
		f.confInfo.status = "ERROR"
		result = fmt.Sprintf(err.Error())
		return result
	}

	err = os.Remove(fName)

	if err != nil {
		f.confInfo.status = "ERROR"
		result = fmt.Sprintf(err.Error())
		return result
	}

	//check if parent dir is now empty
	empty, err := IsEmpty(parentDir)
	//if so is it removable too?
	if empty && f.confInfo.confType == "ProcessDirectoryInput" {
		err = f.checkProcessTickerDir(parentDir)
		if err == nil {
			err = os.Remove(parentDir)
			result = fmt.Sprintf("Successfully removed file: %s as well as empty process ticker directory: %s", fName, parentDir)
			return result
		} else {
			f.confInfo.status = "ERROR"
			result = fmt.Sprintf("Successfully removed file: %s but failed to remove process ticker directory: %s", fName, parentDir)
			return result
		}
	}

	result = fmt.Sprintf("Successfully deleted file: %s", fName)
	return result
}

func (f *CMFilter) checkPath(path string) (ok bool, err error) {
	ok = false
	for _, incPath := range f.includePaths {
		for _, exPath := range f.conf.ExcludePaths {
			if strings.HasPrefix(path, exPath) {
				err = fmt.Errorf("'%s' is under exclude_paths '%s'. Skipping.", path, exPath)
				return false, err
			}
		}
		if strings.HasPrefix(path, incPath) {
			ok = true
		}
	}
	return ok, nil
}

func (f *CMFilter) checkProcessTickerDir(fName string) (err error) {
	empty, err := IsEmpty(fName)
	if !strings.HasPrefix(fName, f.conf.ProcessDir) || filepath.Clean(fName) == filepath.Clean(f.conf.ProcessDir) || !empty || err != nil {
		err = fmt.Errorf("You cannot delete this directory: %s", fName)
		return err
	}
	return err
}

func IsEmpty(name string) (bool, error) {
	f, err := os.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()

	_, err = f.Readdirnames(1) // Or f.Readdir(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err // Either not empty or error, suits both cases
}

//END "remove" Config methods

//BEGIN "return" Action methods
func (f *CMFilter) getConfigFileInfo(name string, configFile toml.Primitive) (configName string, configType string, configCategory string, ticker int) {
	//Get identifiers from config section
	//using ProcessInput's "current" default of 15 seconds for ticker
	ticker = f.confInfo.ticker
	pipeConfig := NewPipelineConfig(nil)
	maker, _ := NewPluginMaker(name, pipeConfig, configFile)
	if maker.Type() != "" {
		if maker.Category() == "Input" {
			mutMaker := maker.(MutableMaker)
			commonConfig, _ := mutMaker.OrigPrepCommonTypedConfig()
			commonInput := commonConfig.(CommonInputConfig)
			ticker = int(commonInput.Ticker)
		}
		return maker.Name(), maker.Type(), maker.Category(), ticker
	}
	return "", "", "", ticker
}

func (f *CMFilter) sendConfigContents(configPath string) (err error) {
	var (
		confString string
		ok         bool
		pluginType string
		section    toml.Primitive
	)

	outBytes := make([]byte, 0, 10000)

	p, err := os.Open(configPath)
	if err != nil {
		return fmt.Errorf("error opening file: " + err.Error())
	}

	fi, err := p.Stat()
	if err != nil {
		return fmt.Errorf("can't stat file: " + err.Error())
	}

	//If path is directory, recursively walk it to find configs
	if fi.IsDir() {
		files := []string{}
		filepath.Walk(configPath, func(path string, f os.FileInfo, err error) error {
			files = append(files, path)
			return nil
		})

	FILES:
		for _, fName := range files {
			if !strings.HasSuffix(fName, ".toml") {
				// Skip non *.toml files in a config dir.
				continue
			}

			for _, excludePath := range f.conf.ExcludePaths {
				if strings.HasPrefix(fName, excludePath) {
					fmt.Printf("'%s' is under excludePath '%s'. Skipping.", fName, excludePath)
					continue FILES
				}
			}

			unparsedConfig := make(map[string]toml.Primitive)
			confString, _ = ReplaceEnvsFile(fName)
			_, err = toml.Decode(confString, &unparsedConfig)

			for _, pluginType = range f.conf.IncludeTypes {
				section, ok = unparsedConfig[pluginType]
				if ok {
					break
				}
			}
			f.confInfo.confName, f.confInfo.confType, f.confInfo.confCategory, f.confInfo.ticker = f.getConfigFileInfo(pluginType, section)
			f.confInfo.fileName = fName
			outBytes = []byte(confString)
			f.outBatch = append(f.outBatch, outBytes...)
			f.count++
			f.confInfo.status = "OK"

			if len(f.outBatch) > 0 {
				f.sendBatch()
			}
		}
	} else {
		//Path is absolute location
		if !strings.HasSuffix(configPath, ".toml") {
			// Skip non *.toml files in a config dir.
			return nil
		}
		//Skip if path is excluded from config
		for _, excludePath := range f.conf.ExcludePaths {
			if configPath == excludePath || filepath.Clean(configPath) == filepath.Clean(excludePath) {
				return nil
			}
		}

		unparsedConfig := make(map[string]toml.Primitive)
		confString, _ = ReplaceEnvsFile(configPath)
		_, err = toml.Decode(confString, &unparsedConfig)

		for _, pluginType = range f.conf.IncludeTypes {
			section, ok = unparsedConfig[pluginType]
			if ok {
				break
			}
		}
		f.confInfo.confName, f.confInfo.confType, f.confInfo.confCategory, f.confInfo.ticker = f.getConfigFileInfo(pluginType, section)
		f.confInfo.fileName = configPath
		f.confInfo.status = "OK"

		outBytes = []byte(confString)
		f.outBatch = append(f.outBatch, outBytes...)
		f.count++
		if len(f.outBatch) > 0 {
			f.sendBatch()
		}
	}
	return nil
}

//END "return" Action methods

func (f *CMFilter) CleanUp() {

}

func init() {
	RegisterPlugin("CMFilter", func() interface{} {
		return new(CMFilter)
	})
}
