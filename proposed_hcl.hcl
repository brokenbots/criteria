// the work flow defines the worflow metadata, the goal is treat the workflow as a collection of files and load them all from one directory
// this is same behavior as terraform, the workflow file can be one or more files
workflow "<name>" {
	name = "" // optional, if not defined, it default to id
	version = "" // optional, if not defined, it default to 0.1

	file = "" // optional, if not defined the steps should be take from the block
	environment "<id>"
}

variable "<name>" {
	description = "" // optional, if not defined, it default to ""
	type = "string" // variable type, it can be string, number, boolean, list, map, etc
	default = any // default value, it can be empty if no default value is needed
}

output "<name>" {
	description = "" // optional, if not defined, it default to ""
	value = any // output value, it can be any type
}

// fenced workflow scoped variable that can be updated during runtime, engine ensure access and locking
shared_variable "<name>" {
	description = "" // optional, if not defined, it default to ""
	type = <varible_type>
	value = any // optional, intial value if not set it defaults to the null or default for the type
}

environment "<type>" "<name>" {
	// environment variables, it can be empty if no variable is needed
	variables = map(string)
	config = map(any) // config shape is defined by environment type, it can be empty if no config is needed
}

// built in adapters, can be used directly in step or can be named and used in a step
adpater "<type>" "<name>" { // plugins, it can be function, http, database, etc
	environment = <type>.<name> // environment is optional, if not defined, it default to workflow environment
	config = map(any) // config shape is defined by adapter type, it can be empty if no config is neededax
}

// a special adapter type has two outcomes, success and failure that must be wired.
subworkflow "<name>" {
	source = "" // directory of the target workflow, it can be local or remote, if not defined, it default to current workflow directory

	environment = <type>.<name> // environment is optional, if not defined, it default to workflow environment
	input = map(any) // input shape is defined by target_workflow, it can be empty if no input is needed

	output = any // output is optional, if not defined, it default target workflow output
}

// target_type is step type: workflow, adatpter, function 
// target type for internl adatpers is `intneral`eg. `internal_shell` for shell adapter
step "<name>" {
	[for_each = map(any)|[] | count = <int> | parallel = [any]] // optional modifiers

	target = adapter.type.name | subworkflow.name | step.name

    environment = <type>.<name> // environment is optional, if not defined, it default to workflow environment
	input = map(any) // input shape, if not set default to step input from previous step, allows using caller.output.key inside to restructure data from previous step

	// a special outcome of return, it will return the output to caller on step return
	outcome "<name>" {
		next = "<step_name>",  
		output = any,  // output is optional, if not defined, it default adapter output'
	} 

	default_outcome = "<outcome_name>" // optional used for adapter like agents that can return invalid outcomes

	output = any // output is optional, if not defined, it default adapter output
}

// switch block for flow control using logic statements.
switch "<name>" {
	condition {
		match = <conditional logic, must return boolean value>
		output = any // optional output, will forward input by default
		next = "<step_name>"
	}

	condition {}

	default {
		next = "<step_name>"
	}
}
