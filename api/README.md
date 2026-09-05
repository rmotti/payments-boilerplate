# Contrato HTTP

`openapi.yaml` e a fonte de verdade da API HTTP. `oapi-codegen.yaml` controla a
geracao dos tipos e do strict server em `internal/transport/http/openapi`.

Execute `make generate` depois de alterar o contrato. O codigo gerado e
versionado e nao deve ser editado manualmente.

