# Dados, retenção e modelo de ameaça

Este documento é operacional. Ele descreve onde os dados ficam, por quanto tempo
devem ficar e como expurgá-los. A decisão que o originou, com as alternativas
consideradas, está em
[ADR 0017](decisions/0017-sensitive-data-and-error-handling.md).

O `SECURITY.md` na raiz trata de reportar vulnerabilidades e de manuseio de
credenciais. Este arquivo trata dos dados que a aplicação persiste.

## Resumo

A aplicação **armazena dados pessoais**. Cada evento recebido da Stripe é
gravado duas vezes na tabela `webhook_events` — os bytes assinados em
`raw_payload` e o mesmo evento parseado em `payload` — e um evento de Checkout
carrega nome, e-mail, telefone e endereços de cobrança e entrega do cliente.

A aplicação **não** armazena dado completo de cartão. Isso decorre do Checkout
hospedado, em que o número nunca chega a este código, e não de nenhum tratamento
feito aqui.

Quem opera uma implantação é o controlador desses dados.

## Inventário

### Contém dados pessoais

| Local | Conteúdo | Acesso | Retenção |
| --- | --- | --- | --- |
| `webhook_events.raw_payload` | Bytes exatos assinados pelo provedor | Conexão direta ao banco | 90 dias após `processed_at` |
| `webhook_events.payload` | Evento parseado | Consumer e conexão direta ao banco | 90 dias após `processed_at` |
| Requisição HTTP de webhook | Evento completo, em memória | Processo da API | Duração da requisição |
| Backups do PostgreSQL | Cópia integral do banco | Conforme o provedor | 30 dias, descarte automático |

`raw_payload` existe porque `jsonb` reordena chaves e descarta formatação, e
portanto não consegue responder depois o que exatamente foi assinado. As duas
colunas cobrem a mesma janela de utilidade e são expurgadas juntas.

### Contém dados pessoais somente se não sanitizado

| Local | Conteúdo | Acesso | Retenção |
| --- | --- | --- | --- |
| `webhook_events.last_error` | Texto do erro, truncado em 500 bytes | Banco e API operacional | Acompanha a linha |
| `outbox_events.last_error` | Texto do erro, truncado em 500 bytes | Banco e API operacional | Acompanha a linha |

Estes campos são devolvidos por `GET /v1/webhook-events` e
`GET /v1/webhook-events/{id}`. Trate-os como superfície pública: veja
[Regras de conteúdo](#regras-de-conteúdo).

### Não contém dados pessoais

| Local | Conteúdo | Observação |
| --- | --- | --- |
| Mensagem no RabbitMQ | `messageId`, `type`, `schemaVersion`, `occurredAt`, `correlationId`, `webhookEventId` | Referência, não cópia — [ADR 0011](decisions/0011-webhook-reception-and-outbox.md) |
| Filas de retry e DLQ | O mesmo da mensagem original | Investigar exige consultar a inbox |
| `outbox_events`, exceto `last_error` | Referência ao evento e estado de publicação | — |
| `orders`, `payments`, `payment_attempts` | Valores, moeda, status e identificadores do provedor | Sem nome, e-mail ou endereço |
| Respostas de `GET /v1/webhook-events*` | Metadados de recepção e entrega | As queries em `db/queries/operations.sql` nunca selecionam os payloads |
| Logs | Método, rota, status, duração, correlação | Não devem conter payload nem credencial |
| Traces OTLP | Atributos de `otelhttp`, nome do serviço, ambiente | Não devem conter payload nem credencial |

O broker estar fora do escopo é deliberado e não é acidente de implementação:
existe **um** sistema sujeito a retenção e controle de acesso sobre dados
pessoais, o PostgreSQL. Publicar o evento completo na mensagem reabriria a
decisão do ADR 0011 e criaria um segundo.

## Política de retenção

### Prazos recomendados

| Dado | Prazo | Contado a partir de |
| --- | --- | --- |
| `webhook_events` em estado terminal (`processed`, `skipped`) | 90 dias | `processed_at` |
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

A retenção da DLQ é operacional, não de privacidade: as mensagens não contêm
dados pessoais. Uma mensagem parada na DLQ é um incidente aberto, e o critério é
resolvê-la, não deixá-la expirar. Purgar a DLQ apaga a única lista de eventos
que ficaram sem efeito.

## Regras de conteúdo

Valem para logs, traces, métricas, `last_error` e respostas públicas. A
verificação automatizada faz parte de E8b.

- Sanitizar **antes** de truncar. O truncamento de 500 bytes feito por
  `errorText`, em `internal/adapters/postgres/repositories/outbox.go`, limita o
  tamanho e não remove nada de sensível: um texto que começa com uma DSN
  continua vazando a senha depois de truncado. A função já é o ponto por onde
  passam tanto o caminho da outbox quanto o `RecordFailure` da inbox.
- Nunca incluir: corpo de requisição ou de webhook; `X-API-Key`;
  `STRIPE_SECRET_KEY` e `STRIPE_WEBHOOK_SECRET`; credenciais em URLs de
  PostgreSQL e RabbitMQ; nome, e-mail, telefone, endereço ou dados de cobrança;
  resposta integral do provedor.
- Métricas não usam IDs, secrets ou valores de entrada livre como labels.
- Nenhuma query da API operacional pode selecionar `raw_payload` ou `payload`.
  As queries em `db/queries/operations.sql` listam colunas explicitamente por
  esse motivo; isso é invariante testada, não convenção.
- Mensagens públicas continuam úteis: identificam a classe do erro e carregam
  correlação suficiente para achar o detalhe nos logs, que são a superfície
  privada.

## Modelo de ameaça

| Atacante | Alcance | O projeto opõe | O projeto não opõe |
| --- | --- | --- | --- |
| Integrador hostil, com chave válida | Toda a API operacional da instalação | Payloads fora das queries operacionais; sanitização de `last_error`; rate limiting | Segregação por tenant — uma chave válida vê todos os eventos |
| Terceiro na internet | Health, webhook e Checkout | Verificação de `Stripe-Signature` sobre os bytes originais antes de desserializar; limite de corpo; allowlist de rotas públicas ([ADR 0010](decisions/0010-route-access-model.md)); rate limiting | Resposta de erro provocada por corpo acima do limite; ataque volumétrico |
| Pessoa com acesso operacional | Banco, broker, logs e traces | Minimização: payloads fora de logs, traces e mensagens; um único sistema a controlar | Mascaramento por coluna; cifragem em nível de aplicação; auditoria de leitura no banco |
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

O repositório entrega o desenho e o procedimento. Não entrega conformidade. São
responsabilidade de quem opera:

- cifragem em repouso do banco e dos backups;
- retenção efetiva dos backups e seu descarte;
- controle de acesso ao PostgreSQL, ao RabbitMQ e à interface de administração;
- execução periódica do expurgo;
- base legal do tratamento e resposta a titulares;
- resposta a incidentes e rotação de credenciais.

## Trabalho derivado

Itens que esta decisão gera e que serão implementados adiante:

| # | Item | Entrega |
| --- | --- | --- |
| 1 | Sanitização dentro de `errorText`, aplicada antes do truncamento | E8b |
| 2 | Teste unitário do sanitizador com DSNs, URLs escapadas, headers e padrões de credencial | E8b |
| 3 | Teste negativo com valores sentinela sobre logs, traces, `last_error` e respostas públicas | E8b |
| 4 | Teste que impede qualquer query operacional de selecionar `raw_payload` ou `payload` | E3/E8b |
| 5 | Procedimento de rotação de `INTEGRATION_API_KEYS`, `STRIPE_SECRET_KEY` e `STRIPE_WEBHOOK_SECRET` | E8b |
| 6 | Expurgo no checklist operacional, com periodicidade recomendada | E9 |
| 7 | Reavaliar expurgo automatizado depois das métricas de E5 | Pós-0.1.0 |
| 8 | Canal privado de vulnerabilidades ativo e testado | E11, bloqueia a 0.1.0 |
