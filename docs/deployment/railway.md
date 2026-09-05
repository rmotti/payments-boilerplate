# Deploy na Railway

Este guia descreve a primeira implantação da API, do worker, do PostgreSQL e
do RabbitMQ em um único projeto Railway. A configuração fica no painel porque
API e worker compartilham o mesmo `Dockerfile`, mas possuem comandos e variáveis
diferentes.

## Topologia

| Serviço | Origem | Comando | Exposição |
| --- | --- | --- | --- |
| `api` | repositório GitHub | `/app/api` | domínio público |
| `worker` | mesmo repositório | `/app/worker` | somente rede privada |
| `Postgres` | serviço Railway | gerenciado | somente rede privada |
| `rabbitmq` | imagem `rabbitmq:4-management` | padrão da imagem | somente rede privada |

## 1. Serviços de dados

Adicione PostgreSQL ao projeto pelo catálogo da Railway.

Crie o serviço `rabbitmq` a partir da imagem `rabbitmq:4-management`, defina
credenciais fortes em `RABBITMQ_DEFAULT_USER` e `RABBITMQ_DEFAULT_PASS` e monte
um volume persistente em `/var/lib/rabbitmq`. Não publique as portas `5672` ou
`15672` sem uma necessidade operacional explícita.

## 2. API

Crie um serviço a partir deste repositório e nomeie-o `api`. A Railway detecta
o `Dockerfile`. Configure:

- start command: `/app/api`;
- pre-deploy command: `/app/migrate up`;
- pre-deploy timeout: `300` segundos;
- healthcheck path: `/health`;
- domínio público gerado pela Railway;
- Wait for CI habilitado.

Variáveis mínimas:

```dotenv
APP_ENV=production
DATABASE_URL=${{Postgres.DATABASE_URL}}
LOG_LEVEL=info
LOG_FORMAT=json
STARTUP_TIMEOUT=60s
SHUTDOWN_TIMEOUT=15s
OTEL_ENABLED=false
```

`PORT` é fornecida automaticamente pela Railway. Quando houver um collector
OTLP no ambiente, habilite `OTEL_ENABLED` e configure
`OTEL_EXPORTER_OTLP_ENDPOINT` com sua URL base HTTP.

Execute migrations apenas no pre-deploy da API. Isso evita que API e worker
tentem migrar o mesmo banco simultaneamente.

## 3. Worker

Crie outro serviço a partir do mesmo repositório e nomeie-o `worker`.
Configure:

- start command: `/app/worker`;
- healthcheck path: `/health`;
- nenhum domínio público;
- Wait for CI habilitado.

Variáveis mínimas:

```dotenv
APP_ENV=production
DATABASE_URL=${{Postgres.DATABASE_URL}}
RABBITMQ_URL=amqp://${{rabbitmq.RABBITMQ_DEFAULT_USER}}:${{rabbitmq.RABBITMQ_DEFAULT_PASS}}@${{rabbitmq.RAILWAY_PRIVATE_DOMAIN}}:5672/
LOG_LEVEL=info
LOG_FORMAT=json
STARTUP_TIMEOUT=60s
SHUTDOWN_TIMEOUT=15s
OTEL_ENABLED=false
```

As referências `${{...}}` são resolvidas pela Railway. O tráfego entre esses
serviços permanece na rede privada do ambiente.

## 4. Fluxo de deploy

O workflow de CI executa geração, análise estática, testes, scanner de
vulnerabilidades e build. Com Wait for CI habilitado, a Railway só inicia o
deploy após a conclusão bem-sucedida desse workflow.

Cadastre a URL pública da API como variável de repositório no GitHub:

```text
RAILWAY_API_URL=https://seu-dominio.up.railway.app
```

Quando a Railway publicar um status de deployment para o GitHub, o workflow
`Post-deploy` consultará `/health` com retries.

## 5. Validação manual

```bash
curl --fail --show-error https://seu-dominio.up.railway.app/health
curl --fail --show-error https://seu-dominio.up.railway.app/openapi.yaml
```

A primeira chamada deve retornar HTTP 200 com `postgres: up`. A segunda deve
retornar o contrato servido pela mesma versão da aplicação.

## Referências

- [Pre-deploy command](https://docs.railway.com/deployments/pre-deploy-command)
- [GitHub autodeploys e Wait for CI](https://docs.railway.com/deployments/github-autodeploys)
- [RabbitMQ com rede privada e volume](https://docs.railway.com/guides/rabbitmq-producers-consumers)
- [Referência de deploy e healthcheck](https://docs.railway.com/config-as-code/reference)
