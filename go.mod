module github.com/mcplocker/provider-google

go 1.27

require github.com/mcplocker/server/pkg/providerkit v0.0.0

// Until providerkit is tagged (pkg/providerkit/v0.1.0 in mcplocker/server),
// it comes from a checkout of the gateway beside this repository.
replace github.com/mcplocker/server/pkg/providerkit => ../../mcplocker/pkg/providerkit
