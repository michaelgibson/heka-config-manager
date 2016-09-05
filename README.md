# Heka Configuration Management
======================

Configuration Management for [Heka](http://hekad.readthedocs.io/en/latest/)....using [Heka](http://hekad.readthedocs.io/en/latest/)

Occasionally there are circumstances where it may not always be possible to leverage the existing configuration management service(assuming there is one) for the environment in which you have deployed Heka.
In this scenario it may be useful to have the ability to deliver and deploy new configurations using the Heka daemon itself.  
This concept is similar to Splunk's [Deployment Architecture](http://docs.splunk.com/Documentation/Splunk/6.1.1/Updating/Deploymentserverarchitecture) which provides a centralized management interface for creating/removing/updating, distributing and applying data collection configurations to the edge "Forwarders".
The CMFilter is intended to be used in conjunction with one or more *DirectoryInput plugin types.

Currently supports:  
[ProcessDirectoryInput](http://hekad.readthedocs.io/en/latest/config/inputs/processdir.html)  
[LogstreamerDirectoryInput](https://github.com/michaelgibson/heka-logstreamer-directory-input)  
[HttpDirectoryInput](https://github.com/michaelgibson/heka-http-directory-input)  
[FilePollingDirectoryInput](https://github.com/michaelgibson/heka-file-polling-directory-input)  

These plugin types are designed to monitor their respective directory structures for changes and dynamically load or unload config files based on changes.

CMFilter
===========

This filter provides the following functionality:
- Accepts new configuration updates using the contents of a Heka message payload.
- Returns contents of existing config files
- Removes specified config files

It will expect the incoming Heka message to have one or more of the following Fields set:
- `Fields[Action]`(string, required)  
	Value must be one of:
	-	"add": Add new or update existing config file.
		Fields["Overwrite"]: (string, optional) - Indicates whether to overwrite an existing config file or fail. Duplication is determined based on name of plugin. Defaults to "false"
	-	"delete": Removes config files indicated by "Filename" Field(required)
		-	Fields["Filename"]: Name of file to be removed. Automatically removes empty ticker directory if under "process_dir"
	- "return": Recursively returns all configurations under "include_paths"

Config:

- cm_tag(string, optional):
		Optional tagging for identifying output from filter.
		This setting creates a new Heka Field called "CMTag" and is given the value of this option. Defaults to "CM"

- include_types([]string, optional):
		PluginTypes to target. Defaults to {"ProcessInput", "LogstreamerInput"}

- exclude_paths([]string, optional):
		Explicitly exclude files or directories from filter "Actions".
		For config paths that absolutely must not be touched.
		Overrides include_paths.

- process_dir(string, optional):
		The location of [ProcessInput](https://hekad.readthedocs.io/en/latest/config/inputs/process.html#config-process-input) configurations that to be processed by a [ProcessDirectoryInput](https://hekad.readthedocs.io/en/latest/config/inputs/processdir.html)
		Defaults to "$share_dir/processes.d"

- logstreamer_dir(string, optional):
		The location of [LogstreamerInput](https://hekad.readthedocs.io/en/latest/config/inputs/logstreamer.html) configurations that to be processed by a [LogstreamerDirectoryInput](https://github.com/michaelgibson/heka-logstreamer-directory-input)
		Defaults to "$share_dir/processes.d"

Example Filter Configuration:

	[cm_filter]
	type = "CMFilter"
	message_matcher = "(Fields[Action] == 'return' || Fields[Action] == 'add' || Fields[Action] == 'delete') && Fields[CMTag] == NIL"
	cm_tag = "CM"
	include_paths = ["/usr/share/heka/processes.d", "/usr/share/heka/logstreamers.d"]
	exclude_paths = ["/data/heka/protoin/"]
	include_types = ["ProcessInput", "LogstreamerInput"]
	process_dir = "/usr/share/heka/processes.d"
	logstreamer_dir = "/usr/share/heka/logstreamers.d"
	use_buffering = true


Example "add" Config Message with syslog LogstreamerInput:

	:Timestamp: 2016-09-01 22:36:43.641743638 +0000 UTC
	:Type: LogstreamerInput
	:Hostname: foo
	:Pid: 28970
	:Uuid: 07d2911d-e6ab-445d-ad4a-676ecae5597b
	:Logger: add_config
	:Payload: [syslog]
	type="LogstreamerInput"
	log_directory = "/var/log"
	file_match = 'auth\.log'
	decoder = "RsyslogDecoder"
	journal_directory = "/var/cache/hekad/logstreamer_dir_inputs"

	:EnvVersion:
	:Severity: 7
	:Fields:
	    | name:"Action" type:string value:"add"
	    | name:"Overwrite" type:string value:"true" representation:"bool"


Example "delete" Config Message:

	:Timestamp: 2016-09-01 22:48:58.753701221 +0000 UTC
	:Type: LogstreamerInput
	:Hostname: foo
	:Pid: 29322
	:Uuid: 113d0841-2dd3-4546-9c04-f4aa10a17362
	:Logger: delete_config
	:Payload:

	:EnvVersion:
	:Severity: 7
	:Fields:
	    | name:"Action" type:string value:"delete"
	    | name:"Filename" type:string value:"/usr/share/heka/logstreamers.d/heka79a52d9b9c5e68595596cc03d984e83a.toml"

Example "return" Config Message

	:Timestamp: 2016-09-01 22:52:03.581118015 +0000 UTC
	:Type: LogstreamerInput
	:Hostname: foo
	:Pid: 29420
	:Uuid: 705f52d2-0589-41b9-b2b8-42139c270b00
	:Logger: return_config
	:Payload:

	:EnvVersion:
	:Severity: 7
	:Fields:
	    | name:"Action" type:string value:"return"

To Build
========

See [Building *hekad* with External Plugins](http://hekad.readthedocs.org/en/latest/installing.html#build-include-externals)
for compiling in plugins.

Edit cmake/plugin_loader.cmake file and add

    add_external_plugin(git https://github.com/michaelgibson/heka-config-manager master)

Build Heka:
	. ./build.sh
