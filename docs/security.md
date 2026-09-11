# Dados, retenção e modelo de ameaça

Este documento é operacional. Ele descreve onde os dados ficam, por quanto tempo
devem ficar e como expurgá-los. A decisão que o originou, com as alternativas
consideradas, está em
[ADR 0017](decisions/0017-sensitive-data-and-error-handling.md).

O `SECURITY.md` na raiz trata de reportar vulnerabilidades e de manuseio de
credenciais. Este arquivo trata dos dados que a aplicação persiste.

## Resumo

A aplicação **armazena dados pessoais**. Cada evento recebido da Stripe é
gravado duas vezes na tabela `webhook_events` — o corpo exato cuja assinatura
foi verificada em `raw_payload` e o mesmo evento parseado em `payload` — e um
evento de Checkout carrega nome, e-mail, telefone e endereços de cobrança e
entrega do cliente.

A aplicação **não** armazena dado completo de cartão. Isso decorre do Checkout
hospedado, em que o número nunca chega a este código, e não de nenhum tratamento
feito aqui.

A organização que determina a finalidade e os meios do tratamento normalmente
atua como controladora. Os papéis de controlador, operador/processador e seus
deveres precisam ser definidos por cada implantação; operar a infraestrutura,
por si só, não determina o papel jurídico.

## Inventário

### Contém dados pessoais

| Local | Conteúdo | Acesso | Retenção |
| --- | --- | --- | --- |
| `webhook_events.raw_payload` | Corpo exato cuja assinatura foi verificada durante a requisição | Conexão direta ao banco | 90 dias após `processed_at` |
| `webhook_events.payload` | Evento parseado | Consumer e conexão direta ao banco | 90 dias após `processed_at` |
| Requisição HTTP de webhook | Evento completo, em memória | Processo da API | Duração da requisição |
| Backups do PostgreSQL | Cópia integral do banco | Conforme o provedor | 30 dias, descarte automático |

`raw_payload` existe porque `jsonb` reordena chaves e descarta formatação. Ele
preserva o corpo recebido para auditoria e diagnóstico depois de a assinatura
ter sido verificada. O header `Stripe-Signature` não é persistido; portanto,
essa coluna sozinha não permite reverificar posteriormente a assinatura nem é
prova criptográfica independente de origem. As duas colunas cobrem a mesma
janela de utilidade e são expurgadas juntas.

### Contém dados pessoais somente se não sanitizado

| Local | Conteúdo | Acesso | Retenção |
| --- | --- | --- | --- |
| `webhook_events.last_error` | Texto do erro, truncado em 500 bytes | Banco e API operacional | Acompanha a linha |
| `outbox_events.last_error` | Texto do erro, truncado em 500 bytes | Banco e API operacional | Acompanha a linha |

Estes campos são devolvidos por `GET /v1/webhook-events` e
`GET /v1/webhook-events/{id}`. Trate-os como superfície pública: veja
[Regras de conteúdo](#regras-de-conteúdo).

### Dados financeiros, pseudônimos ou fornecidos pelo integrador

Estes dados não carregam diretamente nome, e-mail ou endereço, mas podem ser
confidenciais, conter entrada livre ou tornar uma pessoa identificável quando
correlacionados com a Stripe ou com outros registros. Eles não devem ser
classificados genericamente como “sem dados pessoais”.

| Local | Conteúdo | Acesso | Retenção |
| --- | --- | --- | --- |
| `orders` | ID local, produto, quantidade, valor, moeda e `idempotency_key` fornecida pelo integrador | API autenticada e banco | Conforme reconciliação, contabilidade e obrigações da implantação; sem expurgo automático na `0.1.0` |
| `payments` | IDs de pedido/pagamento, provedor, estado, valor e moeda | API autenticada e banco | Acompanha o pedido; sem expurgo automático na `0.1.0` |
| `payment_attempts` | Chave de idempotência, IDs da Stripe, URL de Checkout, expiração e mensagem de falha | API de checkout e banco | Acompanha o pagamento; sem expurgo automático na `0.1.0` |
| Metadados de `webhook_events` | IDs local e do provedor, tipo, estado, horários e erro sanitizado | API operacional autenticada e banco | A linha de auditoria permanece depois do expurgo dos payloads |
| `outbox_events`, mensagens, retry e DLQ | IDs locais, correlação, tipo e estado de entrega | Banco, worker e broker | Até publicação/ack; cópia na DLQ permanece até disposição operacional |

A URL de Checkout pode permitir retomar uma sessão ainda válida e deve ser
tratada como dado confidencial. Chaves de idempotência e mensagens de falha são
entrada livre ou derivada de sistemas externos e podem conter dados pessoais ou
secrets por acidente. E8b deve sanitizar mensagens; a implantação deve impedir
dados pessoais nas chaves e restringir acesso às URLs.

### Superfícies sem cópia do payload do provedor

| Local | Conteúdo | Observação |
| --- | --- | --- |
| Mensagem no RabbitMQ | `messageId`, `type`, `schemaVersion`, `occurredAt`, `correlationId`, `webhookEventId` | Referência, não cópia — [ADR 0011](decisions/0011-webhook-reception-and-outbox.md) |
| Filas de retry e DLQ | O mesmo da mensagem original | Investigar o payload exige consultar a inbox |
| `outbox_events`, exceto `last_error` | Referência ao evento e estado de publicação | Não contém o payload do provedor |
| Respostas de `GET /v1/webhook-events*` | Metadados de recepção e entrega | As queries em `db/queries/operations.sql` nunca selecionam os payloads |
| Logs | Método, rota, status, duração, correlação | Não devem conter payload nem credencial |
| Traces OTLP | Atributos de `otelhttp`, nome do serviço, ambiente | Não devem conter payload nem credencial |

O broker não recebe uma cópia do payload, deliberadamente. Seus identificadores
ainda são dados operacionais e podem ser relacionados à inbox por quem controla
os dois sistemas; por isso o RabbitMQ continua sujeito a controle de acesso e
disposição operacional. Publicar o evento completo na mensagem reabriria a
decisão do ADR 0011 e ampliaria substancialmente a exposição.

## Política de retenção

### Prazos recomendados

| Dado | Prazo | Contado a partir de |
| --- | --- | --- |
| Payloads de `webhook_events` em estado terminal (`processed`, `skipped`) | 90 dias | `processed_at` |
| Metadados restantes de `webhook_events` | Conforme auditoria e obrigações da implantação; sem expurgo automático na `0.1.0` | `received_at` |
| `orders`, `payments` e `payment_attempts` | Conforme reconciliação, contabilidade e obrigações da implantação; sem expurgo automático na `0.1.0` | Criação do registro |
| Backups do PostgreSQL | 30 dias | Data do backup |
| Logs e traces | Conforme o backend da instalação | — |

São recomendações de projeto, não obrigações legais. Cada implantação avalia as
suas.

### Eventos que não são expurgados por prazo

Linhas com status `failed`, `pending` ou `processing` **não** entram no expurgo
por prazo, em nenhuma hipótese automática.

Uma linha `failed` é trabalho pendente. Ela ainda pode ser reprocessada por
`POST /v1/webhook-events/{id}/reprocess`, e apagá-la destrói ao mesmo tempo o
efeito financeiro que não foi aplicado e o registro de que ele ficou faltando.
Expurgar um evento não terminal exige decisão operacional explícita e
registrada, tomada depois de resolver ou aceitar conscientemente a falha.

### A 0.1.0 não tem expurgo automático

A decisão é deliberada e está justificada na seção 4 do ADR 0017: faltam
métrica do volume afetado, teste de sistema que prove que o expurgo não toca
linhas não terminais, e experiência operacional sobre o volume real de uma
instalação. Um job errado apaga eventos financeiros pendentes em silêncio; o
procedimento manual erra do lado de reter demais, que é reversível.

O que não é aceitável é retenção indefinida implícita. A ausência de automação é
uma decisão registrada, com prazo recomendado e procedimento publicado abaixo.

### Procedimento de expurgo

Execute em janela de manutenção, sobre um banco com backup recente.

Antes, verifique o que seria afetado:

```sql
SELECT status, count(*)
FROM webhook_events
WHERE status IN ('processed', 'skipped')
  AND processed_at < now() - interval '90 days'
GROUP BY status;
```

Confirme que nenhuma linha não terminal está velha o bastante para preocupar —
se houver, ela é um incidente aberto, não candidata a expurgo:

```sql
SELECT id, status, received_at, last_error
FROM webhook_events
WHERE status IN ('pending', 'processing', 'failed')
  AND received_at < now() - interval '90 days'
ORDER BY received_at;
```

O expurgo apaga apenas os payloads e preserva a linha de auditoria — recepção,
tipo, status e correlação continuam disponíveis, e o histórico de que o evento
chegou não é destruído:

```sql
UPDATE webhook_events
SET raw_payload = ''::bytea,
    payload     = '{}'::jsonb,
    updated_at  = now()
WHERE status IN ('processed', 'skipped')
  AND processed_at < now() - interval '90 days'
  AND (octet_length(raw_payload) > 0 OR payload <> '{}'::jsonb);
```

Apagar as linhas inteiras é possível, mas exige remover antes a linha
correspondente em `outbox_events`, por causa da chave estrangeira
`outbox_events_webhook_event_fkey`, e perde a trilha de auditoria. Prefira o
`UPDATE` acima, a menos que a instalação tenha motivo escrito para o contrário.

O expurgo no banco primário não alcança os backups. Um dado só desaparece de
fato quando o backup mais antigo que o contém expira — daí a retenção de
backup ser parte da política, e não um detalhe de infraestrutura.

### DLQ

A DLQ não contém o payload nem dados diretos do cliente, mas suas referências
podem ser relacionadas à inbox. A retenção é principalmente operacional: uma
mensagem parada na DLQ é um incidente aberto, e o critério é resolvê-la, não
deixá-la expirar. Purgar a DLQ apaga a única lista de eventos que ficaram sem
efeito.

## Regras de conteúdo

Valem para logs, traces, métricas, `last_error` e respostas públicas. A
verificação automatizada é E8b e está implementada.

- Sanitizar **antes** de truncar. `internal/platform/errsanitize.Sanitize`
  redige credenciais e segredos estruturados e só então corta o texto em
  `errsanitize.MaxLength` (500 bytes), preservando um limite máximo de UTF-8
  válido — nunca corta um rune ao meio. `errorText`, em
  `internal/adapters/postgres/repositories/outbox.go`, é o ponto único por
  onde passam tanto o caminho da outbox (`Retry`/`Failed`) quanto o
  `RecordFailure` da inbox, e é a única função que grava `last_error` em
  qualquer uma das duas tabelas. A API sanitiza novamente na leitura, em
  `operationNullableString`, para proteger valores legados ou escritos
  manualmente antes dessa regra existir.
- O sanitizador redige, no mínimo: credenciais em URLs PostgreSQL,
  RabbitMQ/AMQP e HTTP (`scheme://usuário:senha@host`, incluindo credenciais
  URL-encoded); os headers `Authorization`, `X-API-Key`, `Proxy-Authorization`
  e `Cookie`/`Set-Cookie`; parâmetros e padrões como `password`, `passwd`,
  `token`, `secret`, `api_key` e `access_key`; chaves secretas e restritas da
  Stripe (`sk_live_*`, `sk_test_*`, `rk_live_*`, `rk_test_*`); o webhook secret
  (`whsec_*`); valores cotados em formatos como JSON; e qualquer valor do header
  `Stripe-Signature`, inclusive formatos inválidos. O marcador é sempre
  `[REDACTED]`, estável entre versões, para que um integrador ou operador
  distinga "o erro não disse nada aqui" de "o erro dizia algo e foi removido".
- O mesmo sanitizador é aplicado ao formatar o campo de erro em logs
  estruturados da API, do relay e do consumer, via
  `internal/platform/logging.SanitizedError`, ao texto fatal escrito em
  `stderr` pelos três binários e às mensagens do runner de migrations. O valor
  recuperado por um `recover()` de pânico HTTP recebe o mesmo tratamento.
  Nenhum desses caminhos loga o corpo de uma
  requisição ou de um webhook, o corpo de resposta de um handler, ou a URL de
  Checkout completa. `errors.Is` e `errors.As` continuam operando sobre o
  erro original, não sanitizado: só a representação textual logada passa pelo
  sanitizador.
- Nunca incluir: corpo de requisição ou de webhook; `X-API-Key`;
  `STRIPE_SECRET_KEY` e `STRIPE_WEBHOOK_SECRET`; credenciais em URLs de
  PostgreSQL e RabbitMQ; nome, e-mail, telefone, endereço ou dados de cobrança;
  resposta integral do provedor; URL de Checkout completa.
- Métricas não usam IDs, secrets ou valores de entrada livre como labels.
- `X-Correlation-ID` fornecido pelo cliente só é reaproveitado quando tem forma
  de token ASCII opaco, até 128 bytes, e não contém um padrão conhecido de
  secret. Texto livre ou suspeito é substituído por um identificador aleatório
  antes de alcançar logs; no webhook, isso ocorre também antes de persistir a
  correlação na outbox e publicá-la no broker. Mesmo com essa defesa, não use
  dados pessoais ou credenciais como identificador de correlação.
- Nenhuma query da API operacional pode selecionar `raw_payload` ou `payload`.
  As queries em `db/queries/operations.sql` listam colunas explicitamente por
  esse motivo; isso é invariante testada em
  `internal/adapters/postgres/repositories/operations_sql_test.go`, tanto
  sobre os tipos gerados pelo sqlc quanto sobre o texto-fonte do arquivo
  `.sql`, não apenas por convenção.
- Mensagens públicas continuam úteis: identificam a classe do erro e carregam
  correlação suficiente para achar o detalhe nos logs, que continuam podendo
  mostrar mais contexto operacional do que a resposta pública, mas nunca um
  segredo capturado por este sanitizador.
- O sanitizador não faz reconhecimento geral de PII: nome, e-mail, telefone e
  endereço que cheguem a uma mensagem de erro por acidente não são detectados
  por padrão de texto. A defesa contra isso é não colocar esses dados dentro
  de um erro em primeiro lugar — nenhum caminho de código deste repositório
  interpola `customer_details` ou qualquer campo do payload do provedor em uma
  mensagem de erro.

## Rotação de credenciais

Procedimento para as três credenciais que este projeto trata como secret. Cada
implantação adapta o mecanismo de deploy e de armazenamento (variável de
ambiente, secret manager, etc.); o que segue é a sequência e a ordem que evita
indisponibilidade e reprocessamento incorreto.

### `INTEGRATION_API_KEYS`

Aceita mais de uma chave ativa separada por vírgula, exatamente para permitir
rotação sem indisponibilidade (ver [ADR 0010](decisions/0010-route-access-model.md)).

1. Gere a nova chave com uma fonte criptográfica e pelo menos 256 bits de
   entropia.
2. Adicione a nova chave à variável `INTEGRATION_API_KEYS`, mantendo a antiga,
   e reimplante a API. As duas chaves ficam válidas simultaneamente.
3. Distribua a nova chave ao(s) sistema(s) integrador(es) e confirme que eles
   passaram a usá-la — por exemplo, acompanhando que requisições autenticadas
   continuam chegando sem erros `401` durante a janela de transição.
4. Remova a chave antiga de `INTEGRATION_API_KEYS` e reimplante. A partir daqui
   qualquer requisição com a chave antiga responde `401 Unauthorized`.
5. Se a rotação for motivada por suspeita de vazamento, pule a janela de
   transição: remova a chave comprometida imediatamente e trate a
   indisponibilidade do integrador como consequência aceitável do
   comprometimento.

### `STRIPE_SECRET_KEY`

Autentica chamadas desta aplicação para a Stripe (criação de Checkout Session
e operações relacionadas).

1. No Dashboard da Stripe, crie uma nova chave secreta (ou use o fluxo de
   [roll de chave](https://docs.stripe.com/keys#roll-keys) quando disponível).
2. Atualize `STRIPE_SECRET_KEY` na configuração da implantação e reimplante a
   API. Não há suporte a duas chaves simultâneas neste campo: a troca é
   pontual, então trate isso como uma janela curta de manutenção.
3. Confirme que uma criação de Checkout Session de teste funciona com a nova
   chave antes de revogar a antiga.
4. Revogue a chave antiga no Dashboard da Stripe.
5. Se a rotação for por vazamento, revogue a chave antiga no Dashboard
   **antes** de qualquer outro passo — parar o abuso é mais urgente do que
   manter a API disponível — e só depois configure e implante a nova.

### `STRIPE_WEBHOOK_SECRET`

Verifica a assinatura `Stripe-Signature` de cada evento recebido; é a única
prova de origem que este projeto tem para um webhook (ver
[ADR 0017](decisions/0017-sensitive-data-and-error-handling.md), seção do
modelo de ameaça sobre "provedor comprometido, ou alguém que capture uma
entrega"). Uma rotação errada faz a Stripe redeliverar eventos que a aplicação
passa a rejeitar como assinatura inválida.

1. No Dashboard da Stripe, adicione um novo endpoint de webhook apontando para
   a mesma URL, ou gere um novo signing secret para o endpoint existente, se a
   Stripe oferecer essa opção sem recriar o endpoint.
2. Atualize `STRIPE_WEBHOOK_SECRET` na configuração da implantação e reimplante
   o processo que recebe o webhook. Como só há um secret ativo por vez neste
   campo, eventos entregues entre a geração do novo secret na Stripe e o
   reimplante bem-sucedido são rejeitados com `400 invalid_signature` e
   redelivered pela Stripe (ver política de reentrega da Stripe); eles não são
   perdidos, apenas atrasados.
3. Confirme nas métricas ou nos logs que eventos voltam a ser aceitos
   (`202`/`200`) e que o contador de assinatura inválida parou de crescer.
4. Se um endpoint antigo foi criado em paralelo, remova-o do Dashboard.
5. Se a rotação for por suspeita de vazamento do secret, trate como incidente:
   um secret vazado permite forjar eventos com efeito financeiro. Rotacione
   imediatamente e audite `webhook_events` por eventos suspeitos recebidos
   enquanto o secret esteve comprometido.

## Minimização e retenção de URLs de Checkout e chaves de idempotência

Registro do que E8b cobre para os dois valores de entrada livre ou
confidenciais identificados no inventário
(`payment_attempts.checkout_url` e as chaves de idempotência de `orders` e
`payment_attempts`). Ver também `E8A-9` no backlog oficial abaixo.

**O que já está implementado:**

- A URL de Checkout nunca é escrita em log ou em atributo de trace por nenhum
  caminho de código deste repositório. Ela é gravada apenas em
  `payment_attempts.checkout_url` (acesso: API de checkout e banco) e
  devolvida ao integrador uma única vez, na resposta de
  `POST /v1/orders/{orderId}/checkout`, que é o propósito do endpoint — sem
  isso o integrador não teria como redirecionar o cliente. Um teste negativo
  com valor sentinela cobre este caminho.
- Chaves de idempotência fornecidas pelo integrador (`orders.idempotency_key`,
  `payment_attempts.idempotency_key`) não são usadas como label de métrica e
  não aparecem em logs de acesso; elas trafegam apenas no corpo da requisição,
  no banco e na resposta ecoada ao próprio integrador que a enviou.
- Mensagens de erro que cheguem a mencionar uma chave de idempotência ou uma
  URL de Checkout dentro de `last_error` passam pelo mesmo sanitizador que
  remove credenciais; isso reduz, mas não elimina, o risco de uma chave
  formatada como segredo (por exemplo, contendo `token=` por acidente) vazar
  por esse campo.

**O que fica explicitamente para depois da `0.1.0`:**

- Minimização ativa na origem — por exemplo, recusar ou truncar uma chave de
  idempotência que pareça conter dados pessoais ou um segredo antes de
  persisti-la — não está implementada. A chave é tratada como identificador
  opaco fornecido pelo integrador; impedir que ela carregue dados pessoais é
  responsabilidade da implantação, como já registrado no inventário.
- Expurgo automático ou temporizado de `payment_attempts.checkout_url` (por
  exemplo, apagar a URL depois que a sessão expira em `payment_attempts.expires_at`)
  não está implementado. Hoje a URL acompanha o ciclo de vida do pagamento, sem
  expurgo automático, pelo mesmo motivo geral da seção 4 do
  [ADR 0017](decisions/0017-sensitive-data-and-error-handling.md): falta
  métrica de volume e teste de sistema que prove que o expurgo não atinge
  tentativas ainda em curso. Esse trabalho permanece pós-`0.1.0` (`E8A-9` e
  `E8A-7`).
- O guia operacional consolidado da `0.1.0` documenta a execução supervisionada
  do expurgo de payloads; veja o
  [checklist operacional](deployment/operations.md#expurgo-supervisionado).

## Modelo de ameaça

| Atacante | Alcance | O projeto opõe | O projeto não opõe |
| --- | --- | --- | --- |
| Integrador hostil, com chave válida | Toda a API operacional da instalação | Payloads fora das queries operacionais; sanitização de `last_error`; rate limiting | Segregação por tenant — uma chave válida vê todos os eventos |
| Terceiro na internet | Health e webhook; alcança a fronteira HTTP das demais rotas, mas não passa da autenticação | Verificação de `Stripe-Signature` sobre os bytes originais antes de desserializar; limites de corpo e de headers; allowlist de rotas públicas ([ADR 0010](decisions/0010-route-access-model.md)); documentação desligada por padrão e autenticada fora de development, headers de segurança e ausência de CORS ([ADR 0016](decisions/0016-http-surface-and-client-identity.md)); rate limiting | Resposta de erro provocada por corpo acima do limite; ataque volumétrico |
| Pessoa com acesso operacional | Banco, broker, logs e traces | Minimização: payloads fora de logs, traces e mensagens; evento completo apenas no PostgreSQL | Mascaramento por coluna; cifragem em nível de aplicação; auditoria de leitura no banco |
| Provedor comprometido ou entrega capturada | Injeção de eventos forjados com efeito financeiro | `STRIPE_WEBHOOK_SECRET` como secret; rotação documentada; inbox preserva os bytes recebidos para auditoria posterior | Segunda prova de origem |

### Fora de escopo

- **Multi-tenant.** Uma chave de integração pertence à instalação inteira, não a
  um cliente. Não use uma instalação para servir clientes que não podem ver os
  dados uns dos outros.
- **PCI DSS.** Dados completos de cartão não chegam à aplicação, mas o escopo de
  conformidade é da implantação.
- **LGPD e GDPR.** Este documento diz onde os dados estão e por quanto tempo.
  Base legal, atendimento a titulares, registro de operações e transferência
  internacional são da implantação.
- **Fraude e chargeback.** Não há scoring, regra de risco nem fluxo de disputa.
- **Abuso interno do integrador.** Uma chave válida usada de má-fé não é
  distinguida de uma usada corretamente, além do rate limiting.
- **Disponibilidade sob ataque volumétrico.**

## Responsabilidade da implantação

O repositório entrega o desenho e o procedimento. Não entrega conformidade. A
implantação precisa definir seus papéis jurídicos e atribuir responsabilidade
por:

- cifragem em repouso do banco e dos backups;
- retenção efetiva dos backups e seu descarte;
- controle de acesso ao PostgreSQL, ao RabbitMQ e à interface de administração;
- execução periódica do expurgo;
- base legal do tratamento e resposta a titulares;
- resposta a incidentes e rotação de credenciais.

## Backlog oficial derivado de E8a

Esta tabela é o registro local oficial dos itens gerados por E8a. Enquanto eles
não forem espelhados no rastreador externo, não devem ser descritos como issues
do GitHub. Uma issue futura deve manter o identificador abaixo e apontar para
esta seção.

| ID | Item | Entrega | Status |
| --- | --- | --- | --- |
| E8A-1 | Sanitização dentro de `errorText`, aplicada antes do truncamento | E8b | Concluído — `internal/platform/errsanitize.Sanitize`, usado por `errorText` em `internal/adapters/postgres/repositories/outbox.go` |
| E8A-2 | Teste unitário do sanitizador com DSNs, URLs escapadas, headers e padrões de credencial | E8b | Concluído — `internal/platform/errsanitize/errsanitize_test.go` |
| E8A-3 | Teste negativo com valores sentinela sobre logs, traces, `last_error` e respostas públicas | E8b | Concluído — inclui saída fatal de processo, logs internos, traces, gravação e leitura legada de `last_error`, e respostas HTTP públicas |
| E8A-4 | Teste que impede qualquer query operacional de selecionar `raw_payload` ou `payload` | E3/E8b | Concluído — `internal/adapters/postgres/repositories/operations_sql_test.go` |
| E8A-5 | Procedimento de rotação de `INTEGRATION_API_KEYS`, `STRIPE_SECRET_KEY` e `STRIPE_WEBHOOK_SECRET` | E8b | Concluído — seção "Rotação de credenciais" acima |
| E8A-6 | Expurgo no checklist operacional, com periodicidade recomendada | E9 | Concluído — execução supervisionada semanal no [checklist operacional](deployment/operations.md#expurgo-supervisionado) |
| E8A-7 | Reavaliar expurgo automatizado depois das métricas de E5 | Pós-0.1.0 | Pendente — fora do escopo de E8b |
| E8A-8 | Canal privado de vulnerabilidades ativo e testado | E11, bloqueia a `0.1.0` | Concluído — API do GitHub confirmou `private-vulnerability-reporting.enabled=true` em 2026-09-11; nenhum advisory de teste foi criado ([revisão E11](security-review-0.1.0.md#canal-privado-e-estado-do-github)) |
| E8A-9 | Definir minimização e retenção automática para URLs de Checkout e chaves de idempotência | E8b/pós-0.1.0 | Parcial — minimização e responsabilidade operacional documentadas; expurgo automático/temporizado permanece pós-`0.1.0` |
