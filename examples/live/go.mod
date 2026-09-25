module github.com/joeylking/agent-runtime/examples/live

go 1.27

// The provider modules this example imports live in this repository and
// resolve through the workspace the README builds. They are named here as
// requirements only once they are tagged: a requirement on a version that
// does not exist yet fails every build in the workspace as well.
require github.com/joeylking/agent-runtime v0.1.2
