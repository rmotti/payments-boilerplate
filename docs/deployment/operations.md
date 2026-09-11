# Checklist operacional de implantação

Este é o checklist consolidado para implantar e operar a série `0.1.x`. Use-o
junto do guia da [Railway](railway.md); em outro destino, preserve as mesmas
etapas e substitua apenas os mecanismos da plataforma.

O repositório não provisiona backup, alertas, cofre de secrets, escalonamento
nem resposta a incidentes. Os prazos e limiares abaixo são referências iniciais,
não SLA nem garantia de conformidade. Antes do primeiro deploy, a implantação
precisa atribuir responsáveis, definir RPO/RTO e adaptar essas referências ao
seu volume e às suas obrigações.

## Registro da mudança

Abra um registro para cada implantação e preencha antes de começar:

- [ ] ambiente e janela da mudança;
- [ ] commit, tag e digest imutável da imagem da API e do worker;
- [ ] versão atualmente implantada e versão anterior para rollback;
- [ ] responsável pela execução e pessoa que autoriza rollback/restauração;
- [ ] migrations incluídas e compatibilidade da versão anterior com o schema
  novo;
- [ ] identificador e horário do backup ou snapshot anterior à mudança;
- [ ] links para CI, deployment, dashboard, logs e incidente, quando houver.

Não registre valores de secrets, URLs com credenciais, payloads de webhook nem
URLs completas de Checkout nesse documento.

## Antes do primeiro deploy

### Infraestrutura, acesso e configuração

- [ ] Mantenha PostgreSQL, RabbitMQ, worker e interfaces administrativas apenas
  na rede privada. Publique somente a API e não exponha as portas `5432`, `5672`
  ou `15672`.
- [ ] Monte armazenamento persistente para o RabbitMQ e confirme que ele
  sobrevive à recriação do processo. A persistência do volume não substitui um
  plano de recuperação do broker.
- [ ] Restrinja o acesso operacional ao banco, broker, logs, traces e backups.
  Eles contêm dados pessoais ou identificadores que podem ser correlacionados.
- [ ] Configure cifragem em trânsito e em repouso segundo os recursos do
  provedor. O projeto não cifra colunas no nível da aplicação.
- [ ] Gere credenciais distintas por ambiente. Trate `DATABASE_URL`,
  `RABBITMQ_URL`, `INTEGRATION_API_KEYS`, `STRIPE_SECRET_KEY` e
  `STRIPE_WEBHOOK_SECRET` como secrets.
- [ ] Configure na API as variáveis mínimas do [guia da Railway](railway.md#2-api)
  e, no worker, as do [worker](railway.md#3-worker). Não copie chaves de teste
  para produção nem misture endpoints Stripe de ambientes diferentes.
- [ ] Deixe `DOCS_ENABLED` ausente ou `false`. Se houver uma necessidade
  explícita de documentação no ambiente implantado, habilite-a sabendo que as
  rotas passam a existir e valide que exigem `X-API-Key` fora de development.
- [ ] Defina `TRUSTED_PROXY_CIDRS` somente com faixas realmente controladas pelo
  proxy. Vazio faz a aplicação ignorar `X-Forwarded-For`, que é o default seguro.
- [ ] Mantenha o rate limiting habilitado. Os limites são locais a cada processo;
  aumentar réplicas multiplica a capacidade agregada.
- [ ] Garanta que `DATABASE_MAX_OPEN_CONNECTIONS` do worker comporte
  `CONSUMER_CONCURRENCY + 2`; o próprio processo rejeita uma configuração menor.
- [ ] Se alterar outbox, retry, pool ou sampling, valide a configuração com a
  mesma imagem que será publicada. Em particular,
  `OUTBOX_LEASE_DURATION` precisa cobrir o lote inteiro e
  `METRICS_SAMPLE_TIMEOUT` precisa ser menor que `METRICS_SAMPLE_INTERVAL`.

### Telemetria e alertas

- [ ] Habilite `OTEL_ENABLED=true` na API e no worker, aponte
  `OTEL_EXPORTER_OTLP_ENDPOINT` para um collector alcançável e use nomes de
  serviço distintos. Com o default `false`, nenhum alerta baseado nas métricas
  da aplicação funcionará.
- [ ] Confirme no backend que API e worker exportam séries recentes e que
  `metrics.sampler.age` permanece menor que 60 segundos. Gauges mantêm o último
  valor quando a coleta falha; um painel estável pode estar congelado.
- [ ] Instale os alertas mínimos da tabela abaixo e associe destinatário,
  severidade e canal de escalonamento. Ao agregar múltiplos workers, use `max`
  para gauges compartilhados de backlog e filas, nunca `sum`.

| Sinal | Referência inicial | Resposta |
| --- | ---: | --- |
| `rabbitmq.queue.depth{queue="payments.webhooks.dlq"}` | `> 0` | Abrir incidente e seguir o runbook de DLQ. |
| `webhook.inbox.failed` | `> 0` | Inspecionar a inbox e só reprocessar depois de corrigir a causa. |
| `outbox.relay.abandoned` | aumento `> 0` | Investigar imediatamente; é falha classificada como permanente. |
| `outbox.oldest.age` | `> 60 s` | Verificar relay, RabbitMQ e conectividade. |
| `metrics.sampler.age` | `> 60 s` | Tratar os gauges de estado como desatualizados e verificar o sampler. |
| `payment.webhook.invalid_signature` | crescimento sustentado | Conferir endpoint e secret de assinatura antes de qualquer replay. |
| `http.server.rate_limited{limiter="webhook"}` | aumento `> 0` | Verificar rajada legítima ou abuso; a Stripe reentregará recusas. |
| `http.server.rate_limited{limiter="health"}` | aumento `> 0` | Corrigir a frequência/origem da probe para não retirar processo saudável. |

Os nomes Prometheus e as consultas de exemplo estão em
[métricas](../metrics.md#nomes-no-prometheus). Latência, taxa de erro e capacidade
precisam ser calibradas com dados da própria implantação.

### Backups e restauração

- [ ] Defina RPO, RTO, retenção, região e responsáveis por backup/restauração.
  Como ponto de partida: backup diário do PostgreSQL, retenção automática por
  30 dias e um ponto de recuperação adicional antes de cada migration.
- [ ] Confirme no provedor que backups do PostgreSQL estão habilitados,
  cifrados, restritos e expiram de fato após a retenção escolhida. Um volume ou
  réplica não é, sozinho, um backup independente.
- [ ] Documente o que a plataforma preserva do RabbitMQ e como recuperar sua
  configuração. O PostgreSQL é a fonte de verdade do pipeline; depois de perda
  do broker, mensagens ainda pendentes na outbox podem ser publicadas novamente,
  mas mensagens que só estavam em voo ou na DLQ exigem reconciliação explícita.
- [ ] Faça ao menos mensalmente um ensaio de restauração para um banco isolado,
  nunca por cima do ambiente ativo. Use uma cópia da mesma versão do binário
  `/app/migrate` para executar `status`, confirme a versão do schema e compare
  contagens e estados críticos sem expor payloads.
- [ ] Registre duração, ponto restaurado, verificações realizadas e diferenças.
  Um backup que existe, mas nunca foi restaurado, não comprova recuperabilidade.

No ensaio, restaure em uma instância PostgreSQL isolada e compatível com a
versão de origem, conecte somente um processo operacional com credenciais
temporárias e execute:

```bash
/app/migrate status
```

Depois, consulte ao menos as distribuições de estado que governam o trabalho
financeiro pendente:

```sql
SELECT status, count(*) FROM webhook_events GROUP BY status ORDER BY status;
SELECT status, count(*) FROM outbox_events GROUP BY status ORDER BY status;
SELECT status, count(*) FROM orders GROUP BY status ORDER BY status;
SELECT status, count(*) FROM payments GROUP BY status ORDER BY status;
SELECT status, count(*) FROM payment_attempts GROUP BY status ORDER BY status;
```

Compare os resultados com contagens registradas na origem no instante do
backup. Diferenças compatíveis com o ponto de recuperação são esperadas;
schema inválido, erro de leitura ou ausência inesperada de registros críticos
faz o ensaio falhar. Não inicie API ou worker contra o clone enquanto ele for
apenas evidência de recuperação.

Antes de promover um banco restaurado, mantenha API e worker sem escrita,
valide integridade e estime a janela perdida desde o ponto restaurado. Depois da
troca, reconcilie eventos com a Stripe e monitore duplicações, inbox, outbox e
DLQ; a aplicação tolera redelivery, mas não promete recuperar trabalho ausente
do backup e do provedor.

## Checklist de cada deploy

### Preparação e migrations

- [ ] Exija CI verde para o commit exato e habilite `Wait for CI` nos dois
  serviços.
- [ ] Revise release notes, mudanças de configuração e migrations. Mudanças
  destrutivas ou incompatíveis exigem plano próprio de expansão/contração; não
  dependa de rollback automático de banco.
- [ ] Crie e identifique um backup recuperável imediatamente antes da migration.
- [ ] Execute `migrate status` com o binário da nova imagem e as mesmas
  `DATABASE_URL` e `MIGRATIONS_DIR` do pre-deploy. Registre quais versões estão
  aplicadas e quais pertencem à mudança; investigue qualquer erro ou versão
  inesperada antes de publicar processos.
- [ ] Execute `/app/migrate up` uma única vez no pre-deploy da API. Não configure
  migrations no worker e não permita que duas réplicas migrem em paralelo.
- [ ] Confirme no log que `migrate up` terminou com sucesso e execute `migrate
  status` novamente para verificar que nenhuma migration esperada ficou
  pendente. Se falhar, interrompa a promoção; não inicie API ou worker novos
  sobre schema parcialmente avaliado.

O comando `migrate down` reverte apenas uma migration e migrations atuais
contêm operações que removem tabelas ou colunas. Não o execute automaticamente
em produção. Em incidente, prefira corrigir para frente; se isso não for seguro,
pare as escritas e use o procedimento de restauração aprovado.

### Publicação e readiness

- [ ] Publique API e worker a partir do mesmo commit/digest e confirme nos logs
  de startup `version` e `commit` esperados.
- [ ] Confirme que API e worker encerraram a versão anterior graciosamente antes
  do limite configurado em `SHUTDOWN_TIMEOUT`.
- [ ] Verifique a API pública:

  ```bash
  curl --fail-with-body --show-error "$API_BASE_URL/health"
  ```

  A resposta precisa ser HTTP `200`, `status: ok` e `checks.postgres: up`. A
  rota é uma probe de readiness, não uma prova de que checkout, Stripe, worker
  ou RabbitMQ estão funcionando.
- [ ] De dentro da rede privada, consulte `/health` do worker. A resposta precisa
  ser HTTP `200`, com `postgres: up` e `rabbitmq: up`. O worker não possui outras
  rotas; não lhe atribua domínio público.
- [ ] Confirme que o workflow `Post-deploy` terminou. Ele valida o status dos
  serviços reportado pela Railway e repete a readiness pública da API, mas não
  substitui a validação privada do worker.
- [ ] Valide uma rota autenticada e somente leitura:

  ```bash
  curl --fail-with-body --show-error \
    -H "X-API-Key: $API_KEY" \
    "$API_BASE_URL/v1/webhook-events?limit=1"
  ```

- [ ] Confirme logs sem loop de restart, falha de migration, erro de conexão ou
  rejeição de credencial inesperada. Não copie payloads ou secrets para o
  registro da mudança.
- [ ] Observe por pelo menos uma janela coerente com o volume local que idade e
  quantidade de inbox/outbox não crescem, filas de retry drenam e DLQ permanece
  vazia. Ausência de tráfego não comprova o fluxo ponta a ponta.
- [ ] Quando o ambiente e a política permitirem uma transação sintética, faça um
  pagamento de teste e confirme o estado final assíncrono. Não use fixture
  assinada, chave ou cartão de produção em logs ou tickets.

## Operação recorrente

Esta cadência é um ponto de partida. Documente qualquer frequência diferente e
o risco aceito.

| Frequência | Verificação |
| --- | --- |
| contínua | Readiness, alertas, falhas de exportação, backlog, filas de retry e DLQ. |
| diária | Execução e expiração dos backups; inbox `failed`; outbox envelhecida/abandonada; DLQ. |
| semanal | Prévia e execução supervisionada do expurgo de payloads elegíveis. |
| mensal | Restauração em ambiente isolado e revisão de capacidade/tendências. |
| trimestral | Revisão de acessos, inventário de secrets, responsáveis e necessidade de rotação. |
| após mudança ou incidente | Verificação de logs/dados expostos, atualização do runbook e ajuste dos alertas. |

### Expurgo supervisionado

A versão `0.1.0` não automatiza expurgo. Uma vez por semana, execute em janela
de manutenção a prévia e o `UPDATE` publicados em
[Política de retenção](../security.md#procedimento-de-expurgo), sempre sobre um
banco com backup recente. O procedimento:

- limita-se a payloads de `webhook_events` em `processed` ou `skipped` há mais
  de 90 dias;
- preserva a linha e seus metadados de auditoria;
- nunca inclui automaticamente eventos `pending`, `processing` ou `failed`;
- não remove cópias ainda presentes em backups; elas desaparecem apenas com a
  expiração do backup que as contém.

Registre contagens por status antes e depois. Qualquer evento não terminal com
mais de 90 dias abre um incidente e não é candidato a expurgo. O SQL é
idempotente, mas a conferência humana e o backup continuam obrigatórios.

Não há na `0.1.0` procedimento seguro suportado para apagar automaticamente
`payment_attempts.checkout_url` ou chaves de idempotência. Esses valores seguem
a retenção do pagamento; não improvise um `UPDATE`, porque a URL e a chave
participam da recuperação idempotente do Checkout. A minimização/expiração
específica permanece trabalho posterior registrado em
[Dados, retenção e modelo de ameaça](../security.md#minimização-e-retenção-de-urls-de-checkout-e-chaves-de-idempotência).

### Rotação de credenciais

Mantenha proprietário e data da última rotação de cada secret. Execute a
cadência definida pela implantação e rotacione imediatamente diante de suspeita
de exposição. Siga a ordem detalhada em
[Rotação de credenciais](../security.md#rotação-de-credenciais):

- `INTEGRATION_API_KEYS` aceita chave nova e antiga durante uma transição sem
  indisponibilidade;
- `STRIPE_SECRET_KEY` aceita somente uma chave na aplicação, portanto a troca
  requer uma janela curta e validação antes de revogar a anterior;
- `STRIPE_WEBHOOK_SECRET` aceita somente um secret; monitore assinatura inválida
  e recepção dos eventos durante a troca.

Rotacione também credenciais de PostgreSQL, RabbitMQ, collector e plataforma
segundo os recursos dos respectivos provedores. Como o comportamento de
sobreposição dessas credenciais não é implementado por este repositório, teste
o procedimento por ambiente antes de usá-lo em produção. Em vazamento, revogue
a credencial comprometida antes de priorizar disponibilidade.

## Runbooks de incidente

### API ou PostgreSQL indisponível

1. Abra o incidente, preserve horário, versão/commit e correlação; não copie
   payload ou secret.
2. Confira `/health`, logs do processo, métricas de pool e estado do PostgreSQL.
   HTTP `503` com `postgres: down` é readiness negativa, sem detalhe sensível.
3. Interrompa novos deploys e mudanças de schema. Se o banco só estiver
   temporariamente indisponível, recupere a dependência e deixe os clientes e a
   Stripe repetirem segundo suas políticas.
4. Se houver corrupção ou perda, mantenha API e worker sem escrita e siga a
   restauração aprovada. Registre o ponto recuperado e reconcilie a janela
   perdida com a Stripe antes de encerrar.

### RabbitMQ ou worker indisponível

1. Observe que a API pode continuar com HTTP `200`: sua readiness verifica o
   PostgreSQL, não o RabbitMQ. O worker deve responder `503` com `rabbitmq: down`.
2. Verifique runtime/restarts do worker, conexão e volume do broker. Não apague
   ou recrie filas que já carregam mensagens financeiras.
3. Enquanto o broker estiver fora, webhooks aceitos permanecem na inbox/outbox.
   Recupere broker e worker e acompanhe `outbox.oldest.age`, `outbox.pending` e
   profundidade das filas até drenarem.
4. Não reprocese em massa para “acelerar” a recuperação: o relay já repete
   falhas transitórias sem limite de tentativas.

### Inbox com falha, outbox abandonada ou DLQ

Uma mensagem na DLQ é um incidente aberto. Ela contém uma referência à inbox,
não o payload da Stripe, e não deve ser purgada por idade. Um abandono do relay
é diferente: deixa a outbox em `failed` no PostgreSQL e não cria, por si só,
uma mensagem na DLQ.

1. Liste o trabalho com falha sem expor payloads:

   ```bash
   curl --fail-with-body --show-error \
     -H "X-API-Key: $API_KEY" \
     "$API_BASE_URL/v1/webhook-events?status=failed&limit=100"
   ```

2. O filtro HTTP considera o status da inbox. Uma outbox `failed` pode estar
   ligada a uma inbox ainda `pending` e não aparecer nessa resposta. Consulte
   somente seus metadados por uma conexão PostgreSQL operacional restrita:

   ```sql
   SELECT e.id AS webhook_event_id,
          e.status AS webhook_status,
          o.id AS outbox_id,
          o.status AS outbox_status,
          o.attempts,
          o.last_error
   FROM webhook_events e
   JOIN outbox_events o ON o.webhook_event_id = e.id
   WHERE o.status = 'failed'
   ORDER BY o.updated_at;
   ```

   Não selecione `raw_payload` nem `payload`.
3. Correlacione `webhookEventId`, `lastError`, outbox, logs e, quando existir, a
   mensagem na fila `payments.webhooks.dlq`. A API lista no máximo os 100
   eventos mais recentes; use o acesso restrito ao banco quando o incidente não
   aparecer ali.
4. Classifique a causa. Divergência de valor, moeda, provider ou identificadores
   é terminal e exige reconciliação humana; repetição cega não a corrige.
5. Corrija a causa e reprocese apenas o evento escolhido. O mesmo endpoint aceita
   inbox `failed` **ou** outbox `failed`:

   ```bash
   curl --fail-with-body --show-error -X POST \
     -H "X-API-Key: $API_KEY" \
     "$API_BASE_URL/v1/webhook-events/$WEBHOOK_EVENT_ID/reprocess"
   ```

   HTTP `202` significa reenfileirado, não processado. `409` significa que o
   trabalho já deixou o estado reprocessável; inspecione novamente em vez de
   repetir em loop.
6. Confirme o estado comercial esperado, inbox `processed`/`skipped`, outbox
   `published` e ausência de nova falha. O replay reutiliza a mensagem original
   e registra `replayCount`/`lastReplayedAt`.
7. O endpoint de replay não remove a cópia já existente na DLQ, e a `0.1.0` não
   fornece comando para apagar uma mensagem dessa fila por ID. O RabbitMQ
   também não oferece remoção arbitrária por `messageId` como operação de fila.
   Se houver qualquer outro item pendente, mantenha a cópia resolvida, registre
   seu ID como reconciliado no incidente e não purgue a fila.
8. Uma purga total só é aceitável como disposição em lote: pause o worker para
   impedir novas entradas, confirme que a profundidade estabilizou, inventarie
   **cada** mensagem atual, comprove e registre a reconciliação de todas elas,
   obtenha a aprovação operacional definida pela implantação e então purgue a
   fila pela interface administrativa restrita. Confirme profundidade zero e
   retome o worker. Se qualquer item não puder ser provado como resolvido, não
   purgue. Esse procedimento apaga a lista do broker, não os registros da inbox.
9. Registre causa, decisão financeira, evidência e melhoria preventiva. Enquanto
   uma cópia resolvida precisar permanecer ao lado de trabalho pendente, o
   alerta de profundidade continuará aberto com essa limitação anotada.

### Credencial possivelmente exposta

1. Revogue primeiro a credencial comprometida no sistema que a emitiu; não a
   inclua em tickets, logs ou mensagens.
2. Siga o runbook específico de rotação e reimplante apenas os processos que
   consomem a credencial.
3. Delimite o período de exposição e audite ações nesse intervalo. Vazamento do
   webhook secret exige procurar eventos suspeitos na inbox e reconciliá-los
   com a Stripe.
4. Verifique histórico do repositório, artefatos, logs e backend de telemetria;
   remova cópias quando possível e registre limitações de retenção.

## Rollback

Rollback de binário e rollback de dados são decisões diferentes:

1. Pare a promoção e capture logs, métricas, versão e migrations aplicadas.
2. Se o schema novo for retrocompatível, publique API e worker anteriores pelo
   digest registrado e execute novamente as verificações de readiness e
   pós-deploy.
3. Se a versão anterior não for compatível, não a inicie sobre o schema novo.
   Prefira uma correção para frente. Não use `migrate down` sem revisão porque
   os downs atuais podem remover tabelas, colunas e dados.
4. Quando a única recuperação segura for restaurar dados, interrompa escritas,
   restaure o backup em destino isolado, valide-o e só então faça a troca
   controlada. Documente perda estimada e reconcilie com a Stripe.
5. Depois de qualquer caminho, confirme worker privado, API pública, versão dos
   dois processos, fluxo assíncrono, backlog e DLQ. Mantenha o incidente aberto
   até o processamento represado estabilizar.

## Encerramento

- [ ] Todos os checks de API e worker estão prontos e na versão esperada.
- [ ] Migrations e backup/restauração têm evidência anexada ao registro.
- [ ] Alertas estão ativos, entregues ao responsável e sem silenciamento
  residual da janela.
- [ ] Inbox, outbox, retry e DLQ estão no estado esperado, ou há incidente aberto
  com responsável.
- [ ] Nenhum secret, payload, dado pessoal ou URL de Checkout foi incluído nas
  evidências.
- [ ] Desvios, rollback e lições foram registrados; documentação e limiares
  foram ajustados quando necessário.

## Referências

- [Deploy na Railway](railway.md)
- [Métricas e referências operacionais](../metrics.md)
- [Dados, retenção, expurgo e rotação](../security.md)
- [Contrato da API operacional](../api.md#get-v1webhook-events)
- [ADR 0013: retry e DLQ](../decisions/0013-consumer-transactions-transitions-and-retry.md)
- [ADR 0017: dados sensíveis e retenção](../decisions/0017-sensitive-data-and-error-handling.md)
