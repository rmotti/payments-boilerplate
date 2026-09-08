# ADR 0014: composição executável importável e fronteiras do teste ponta a ponta

- Status: aceito
- Data: 2026-09-07
- Implementação: composição extraída na primeira entrega da Fase 4

## Contexto

A Fase 4 precisa de provas de sistema completo: um teste que crie um pedido
pela API, receba um webhook assinado, deixe o relay publicar a mensagem e
verifique que o consumer aplicou o efeito. Esse teste precisa subir a API e o
worker no mesmo processo do teste, com o mesmo desenho que os binários usam.

Nada disso era possível com a composição onde ela estava. Toda a montagem da
API vivia em `cmd/api/main.go` e toda a do worker em `cmd/worker/main.go`, em
pacotes `main`. Um pacote `main` não é importável, e `run()` não era exportada.
Um teste em `tests/e2e` não tinha como chamá-la.

As saídas possíveis eram três. Recompilar os binários e executá-los como
processos filhos, o que torna o teste lento, dependente de artefatos de build e
cego ao estado interno. Reescrever a montagem dentro do próprio teste, o que
cria uma segunda composição que diverge da real exatamente quando importa.
Ou tornar a composição importável, que é o que este ADR decide.

Há ainda uma questão específica da Stripe. O adapter de checkout fala com a
Stripe pelo SDK oficial, e um teste ponta a ponta não pode depender da rede
externa. A saída mais óbvia seria uma variável de ambiente apontando o SDK para
um servidor local. Ela é recusada abaixo.

## Decisão

### A composição vive em `internal/runtime/api` e `internal/runtime/worker`

Cada pacote expõe `Run(ctx context.Context, cfg config.Config, opts Options) error`
com toda a montagem do processo: logger, telemetry, banco, broker, serviços de
aplicação, adapters, servidor HTTP e goroutines.

`cmd/api` e `cmd/worker` ficam com o que é de fato responsabilidade de um
processo: carregar a configuração, transformar sinais em contexto cancelado e
mapear falha em código de saída. Os dois `main` passaram a ter pouco mais de
trinta linhas cada.

A consequência que justifica a mudança é que binário e teste passam a executar
a mesma função. Não existe uma composição "de teste" que possa divergir da que
roda em produção, porque só existe uma.

O nome do serviço e o endereço padrão de cada processo passam a ser constantes
do pacote de runtime, e não literais no `main`. Um teste que carrega
configuração carrega a mesma identidade que o binário carrega.

### As dependências substituíveis são portas da aplicação, não configuração

`Options` tem todos os campos opcionais: `Options{}` produz exatamente o
processo que o binário produz. Os campos existentes são o listener HTTP, as
portas de provider e o logger.

O critério para um campo entrar em `Options` é ser uma dependência que sairia
da máquina. `PaymentProvider` (`payments.Provider`) e `WebhookVerifier`
(`webhooks.Verifier`) na API, `Interpreter` (`consumer.Interpreter`) no worker.
Todos são as portas estreitas que a própria aplicação já define — nenhuma
interface nova foi criada para o teste.

Isso é deliberadamente diferente de uma variável de ambiente que aponte o SDK
da Stripe para um fake. Uma variável dessas existiria no binário de produção,
seria documentada, e uma configuração errada a apontaria para um servidor
arbitrário em produção. A porta injetada não existe fora do processo de teste:
não há como configurá-la em produção porque ela não é configuração.

Quando `PaymentProvider` é substituído, `ValidateStripe()` deixa de ser
exigido — nada chegará ao SDK. O segredo do webhook continua validado, porque a
verificação de assinatura roda igual nos dois casos.

### O listener HTTP pode ser injetado já aberto

`httpserver.Config` ganhou o campo `Listener`. Quando presente, o servidor faz
`Serve` nele em vez de `ListenAndServe` no endereço.

O motivo é uma corrida, não conveniência. Um teste que precisa saber a porta
teria que abrir um listener na porta zero, ler o endereço, fechá-lo e pedir ao
servidor que abrisse o mesmo endereço de novo. Entre o fechamento e a
reabertura, outro processo pode tomar a porta. Entregar o socket já aberto
elimina a janela: o teste lê o endereço do mesmo socket que o servidor atende.

O chamador continua dono do listener. `Run` o atende e o fecha junto com o
shutdown do servidor, mas nunca o fecha em um caminho onde o servidor não
chegou a rodar.

### A Stripe é provada por dois testes, não por um

O mapeamento e o fluxo são garantias diferentes e ficam em camadas diferentes.

O **teste de adapter** sobe um servidor HTTP falso e aponta o SDK para ele
dentro do teste. Ele verifica o request que o SDK realmente produz: os valores
vindos do servidor, a metadata que liga a sessão ao agregado local e a chave de
idempotência. É o que prova o mapeamento, e é a única camada onde a
serialização do SDK está no caminho.

O **teste ponta a ponta** injeta `PaymentProvider` e não tem HTTP nenhum entre
a aplicação e o provider. É o que prova o fluxo: pedido, checkout, webhook,
inbox, outbox, relay, consumer, efeito.

O webhook é a exceção que continua real nos dois. O evento é assinado
localmente com um `STRIPE_WEBHOOK_SECRET` de teste, então a verificação de
assinatura é exercitada de verdade — inclusive a rejeição de uma assinatura
inválida. Assinatura é justamente a fronteira que um fake não deve simular.

## Consequências

- Existe uma composição por processo, usada por binário e por teste.
- `cmd/*` deixa de conter lógica. O que sobra lá é configuração, sinal e saída.
- Encerrar o contexto encerra tudo: cada recurso aberto pela composição é
  liberado antes de `Run` retornar, e o socket volta a ficar livre.
- Um teste ponta a ponta não precisa de rede externa nem de variável de
  produção para evitá-la.
- `Options` é o ponto de pressão a vigiar. Cada campo novo é uma diferença
  possível entre o que o teste roda e o que produção roda, então o critério
  — porta da aplicação que sairia da máquina — vale para as próximas também.
