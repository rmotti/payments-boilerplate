# Revisão de segurança pré-0.1.0

- Data: 2026-09-11
- Base revisada: `e8b5a7b9275d3b7281c4f3879e729659bca56126`
- Resultado: nenhum achado bloqueante permanece aberto; a publicação ainda
  depende da CI verde sobre o diff final integrado.

Este documento registra a revisão E11. Ele não é certificação de PCI DSS,
LGPD, GDPR nem teste de intrusão. O modelo de ameaça e os limites assumidos
continuam em [Dados, retenção e modelo de ameaça](security.md).

## Escopo e método

Foram revisados, por leitura de código e testes existentes:

- autenticação, allowlist pública, documentação HTTP, identidade do cliente,
  rate limiting, limites e timeouts do servidor;
- recepção e assinatura de webhooks, inbox/outbox, retry, DLQ, idempotência e
  transições financeiras;
- consultas operacionais, erros públicos, logs, traces, métricas, persistência
  de payloads e credenciais;
- configuração, PostgreSQL, RabbitMQ, Stripe, Docker/Compose e os workflows de
  CI, CodeQL, release, caos e pós-deploy;
- política de reporte privado e o checklist operacional, inclusive backup,
  restauração, expurgo supervisionado e disposição de DLQ.

A revisão procurou especialmente quebra de autenticação, falsificação/replay de
webhook, cobrança duplicada, exposição de payload ou secret, SQL injection,
consumo irrestrito de recursos, confiança indevida em proxies e risco da cadeia
de fornecimento.

## Achados corrigidos

### SR-01 — referências mutáveis em GitHub Actions (alta)

Todos os workflows usavam tags principais como `actions/checkout@v7` e
`docker/build-push-action@v7`. Uma tag pode mudar depois da revisão; no workflow
de release, código alterado teria permissões de `contents: write`,
`packages: write`, `id-token: write` e `attestations: write`.

Correção: todas as actions foram fixadas nos commits retornados pelo GitHub para
as tags declaradas em 2026-09-11. O comentário ao lado do SHA preserva a versão
humana. Não resta referência `uses:` por tag.

### SR-02 — cabeçalhos sem teto explícito antes do rate limiting (média)

O servidor dependia do limite padrão do `net/http`. Cabeçalhos são recebidos
antes que os middlewares da aplicação possam aplicar rate limiting, então uma
requisição podia exigir uma alocação desnecessariamente grande mesmo sem chegar
à autenticação.

Correção: `http.Server.MaxHeaderBytes` passou a ser `32 KiB`. Os headers deste
contrato são escalares e pequenos; o valor ainda deixa margem para propagação de
trace. Há teste que fixa esse limite.

### SR-03 — correlation id livre atravessava fronteiras de confiança (baixa)

Um `X-Correlation-ID` fornecido pelo cliente, se tivesse até 128 bytes, era
aceito como texto livre. O valor aparece em resposta e log e, no webhook, é
persistido na outbox e publicado no RabbitMQ. Isso permitia poluição de
evidências e transporte acidental de valores com forma conhecida de secret.

Correção: valores fornecidos agora precisam ser tokens ASCII opacos, começar
por alfanumérico, conter apenas alfanuméricos, ponto, sublinhado, dois-pontos ou
hífen, ter no máximo 128 bytes e não acionar o sanitizador de secrets. Qualquer
outro valor é substituído por um identificador aleatório gerado pelo servidor.
Testes cobrem preservação de token válido e substituição de texto livre, valor
grande, chave Stripe e webhook secret.

## Controles confirmados

- Toda operação de negócio ou operacional exige `X-API-Key`; somente health e
  webhook estão na allowlist pública. A comparação usa tags HMAC de tamanho
  fixo e visita todas as chaves configuradas.
- A documentação fica ausente por padrão e exige a mesma autenticação fora de
  `development` quando habilitada. Não há CORS implícito e todas as respostas
  recebem `no-store`, `nosniff`, política de referrer, frame e CSP.
- O webhook verifica `Stripe-Signature` sobre os bytes originais, com tolerância
  temporal do SDK, antes de persistir um evento como válido. Resposta `2xx` só
  ocorre depois da persistência durável ou para duplicata/ignorado já tratado.
- Corpos têm limites separados (`64 KiB` na API e `512 KiB` no webhook), o
  servidor possui timeouts e os limitadores mantêm capacidade de memória
  limitada. `X-Forwarded-For` só é aceito de CIDRs explicitamente confiáveis.
- Preço e moeda vêm do catálogo do servidor. Índices, transações, idempotência,
  publisher confirms e transições monotônicas sustentam o modelo at-least-once
  sem cobrança duplicada causada pela aplicação.
- O broker recebe referência ao evento, não o payload. Queries operacionais não
  selecionam `raw_payload` ou `payload`; `last_error` é sanitizado ao gravar e
  novamente ao ler.
- Logs de acesso não incluem query string, corpo, headers ou URL de Checkout;
  erros internos são sanitizados e respostas inesperadas são genéricas.
- A imagem final é distroless e executa como `nonroot`. O release produz SBOM e
  provenance, e os workflows usam permissões explícitas.

## Canal privado e estado do GitHub

Em 2026-09-11, consultas autenticadas à API do GitHub confirmaram:

- `private-vulnerability-reporting.enabled = true` para
  `rmotti/payments-boilerplate`;
- o link em `SECURITY.md` aponta para
  `security/advisories/new` desse mesmo repositório;
- secret scanning e push protection ativos, sem alerta aberto;
- nenhum alerta aberto de code scanning;
- CI e CodeQL concluídos com sucesso para o commit-base exato da revisão.

Nenhum relato de teste foi submetido: isso criaria um advisory real e não era
necessário para validar a configuração. Portanto, o fluxo posterior ao envio e
os prazos humanos de resposta da política não foram exercitados.

## Verificações executadas

| Verificação | Resultado |
| --- | --- |
| `go tool govulncheck ./...` | passou; nenhuma vulnerabilidade alcançável conhecida |
| `go vet ./...` | passou |
| `go tool golangci-lint run` | passou; zero achados |
| testes focados de auth, config, sanitização e HTTP com `-race` | passaram |
| busca local por formatos de chave privada, GitHub e Stripe | nenhum secret real encontrado |
| `docker compose config --quiet` | passou |
| API do GitHub: private reporting, code scanning e secret scanning | configuração ativa; zero alertas abertos |

`go test ./...` compilou e passou nos pacotes que não abrem sockets, mas a
execução completa local não pode ser declarada verde neste ambiente: testes de
HTTP, RabbitMQ e caos que chamam `listen tcp 127.0.0.1:0` foram recusados pelo
sandbox com `operation not permitted`. Não houve falha funcional nesses testes.
A CI do diff final é o gate que fecha essa limitação.

## Riscos residuais e decisões da implantação

- Não há isolamento multi-tenant, antifraude nem mitigação de ataque
  volumétrico. O rate limiting é por processo e multiplica com réplicas.
- Payloads Stripe contêm dados pessoais em duas colunas até o expurgo manual. A
  `0.1.0` não cifra colunas nem automatiza expurgo; o checklist exige execução
  semanal supervisionada, sem tocar estados não terminais.
- TLS, cifragem em repouso, segregação de rede, auditoria de acesso e ciclo de
  backups pertencem à implantação. O checklist exige backup diário com retenção
  inicial de 30 dias e ensaio mensal de restauração isolada.
- A `0.1.0` não remove uma mensagem individual da DLQ por `messageId`. O
  checklist proíbe purga por idade, mantém a cópia reconciliada quando houver
  outros itens e só permite purga total após inventário, reconciliação e
  aprovação de cada mensagem.
- Dependabot alerts e security updates estavam desabilitados no GitHub no dia da
  revisão. `govulncheck` continua sendo gate de CI, mas uma vulnerabilidade nova
  só é detectada na próxima execução; habilitar alertas contínuos é recomendado.
- Imagens base e de serviços usam tags de patch, não digests. A imagem publicada
  recebe digest, SBOM e provenance, mas reconstruir o mesmo commit em outro dia
  pode resolver bases diferentes. Fixar e renovar digests é endurecimento de
  cadeia de fornecimento recomendado depois da `0.1.0`.

## Parecer de release

Não há vulnerabilidade conhecida ou desvio de desenho aberto que bloqueie a
`0.1.0`. O parecer é favorável depois de CI e CodeQL verdes sobre o diff final e
da conferência do checklist operacional pela implantação que fará o release.
