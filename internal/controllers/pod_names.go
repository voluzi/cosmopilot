package controllers

var deterministicChainNodePodSuffixes = [...]string{
	"-init-data",
	"-config-generator",
	"-genesis-init",
	"-write-file",
	"-download-genesis",
	"-create-validator",
	"-tmkms-generate-identity",
	"-tmkms-vault-upload",
}

// IsDeterministicChainNodePodName reports whether podName is the main ChainNode pod or one of the
// one-shot helper pods whose deterministic name is reserved for that ChainNode.
func IsDeterministicChainNodePodName(podName, nodeName string) bool {
	if podName == nodeName {
		return true
	}
	for _, suffix := range deterministicChainNodePodSuffixes {
		if podName == nodeName+suffix {
			return true
		}
	}
	return false
}
