# ADR 0018: rate limiting das operações públicas e sujeitas a abuso

- Status: aceito
- Data: 2026-09-08
- Implementação: aplicada na entrega E7 da Fase 4

## Contexto

O [ADR 0010](0010-route-access-model.md) fechou o acesso às rotas de negócio e
o [ADR 0016](0016-http-surface-and-client-identity.md) fechou a superfície HTTP
e entregou uma identidade de cliente confiável. Falta o terceiro controle: nada
limita **quantas** requisições um chamador faz.

A ausência aparece em quatro lugares distintos, e eles não têm a mesma forma.

**Rotas anônimas alcançáveis pela internet.** `/health` responde a qualquer um.
O webhook também: ele é público na rede por construção, porque a Stripe não
tem como apresentar credencial. Uma dessas rotas sob carga hostil consome
conexões, goroutines e, no caso do webhook, uma verificação de assinatura
sobre até 512 KiB por requisição.

**Autenticação não é limite.** Uma chave válida hoje pode criar pedidos e
sessões de checkout na velocidade que a rede permitir. Cada checkout é uma
chamada à API da Stripe, que tem custo e limite próprio: um laço acidental no
backend do integrador esgota a cota da instalação antes que alguém perceba.

**Credencial inválida custa caro.** Uma requisição sem chave ainda paga o
parsing OpenAPI e a comparação em tempo constante contra todas as chaves
ativas. Sem limite, um atacante escolhe quanto trabalho a aplicação faz por ele.

**Um limitador ingênuo é pior que nenhum.** Duas armadilhas concretas: chavear
por `X-Forwarded-For` lido diretamente entrega a cada cliente o próprio balde,
e guardar baldes num mapa indexado por algo que o cliente escolhe transforma o
controle de abuso na forma mais barata de esgotar a memória do processo.

## Decisão

### Algoritmo: token bucket, sem goroutine e com relógio injetável

Cada balde tem um **burst**, que é quantas requisições passam em sequência a
partir do balde cheio, e um **intervalo**, que é quanto tempo leva para o burst
inteiro se recompor. A taxa sustentada é burst por intervalo, e um token vale
`intervalo / burst`.

O refill é preguiçoso: acontece na leitura, a partir do tempo decorrido desde a
última, e não há nenhuma goroutine de manutenção. O relógio é injetado, o que
permite provar refill e expiração exatamente, sem `sleep` na suíte.

Duas propriedades foram escolhidas deliberadamente:

- **uma recusa não consome crédito.** Um cliente que insiste em laço não empurra
  o próprio `Retry-After` para frente;
- **um balde ocioso acumula no máximo um burst.** Ficar calado por um dia não
  compra o consumo do dia inteiro de uma vez.

Token bucket foi preferido a janela fixa, que permite o dobro do limite na
virada da janela, e a janela deslizante com log de timestamps, cujo custo de
memória cresce com o tráfego — exatamente o que este ADR precisa evitar.

### Identidade: endereço canônico e impressão da credencial

Para negócio, operações, documentação e caminhos desconhecidos, o limitador
grosseiro é chaveado pelo endereço que o `ClientAddressResolver` do
[ADR 0016](0016-http-surface-and-client-identity.md) já resolve. Ele não lê
`X-Forwarded-For` por conta própria: `TRUSTED_PROXY_CIDRS` decide se o header
vale, e quem chega direto da internet é atribuído ao próprio peer TCP. O
agrupamento é o daquele ADR: IPv4 por endereço, IPv6 por `/64`.

O limitador autenticado é chaveado por uma **impressão criptográfica** da
credencial: a mesma tag HMAC-SHA-256 que o verificador já mantém, derivada de
um segredo aleatório por processo. A chave em texto puro nunca é usada como
chave de mapa, nunca é armazenada e a impressão não é reversível nem
correlacionável entre processos ou reinícios.

O ponto decisivo é o que acontece com uma credencial **inválida**: ela não
recebe impressão e portanto não cria balde. Sem isso, qualquer pessoa mintaria
baldes ilimitados variando um header. Essas requisições permanecem contadas
apenas pelo limitador grosseiro, e a autenticação as recusa com `401` como
sempre recusou.

### Política por classe de rota

| Classe | Chave do balde | Default | Motivo |
| --- | --- | --- | --- |
| Grosseiro (negócio, operações, documentação e caminhos desconhecidos) | endereço do cliente | 1 200 / min | Primeira barreira por origem, antes de parsing e autenticação |
| Negócio e operações | impressão da credencial | 600 / min | Um integrador legítimo opera folgado; um laço acidental é contido |
| Webhook | balde global único | 600 / min | Executado antes da leitura do corpo; o IP da Stripe não é identidade confiável |
| Health | endereço do cliente, balde próprio | 120 / min | Probe não pode ser derrubado pelo tráfego comum |
| Documentação | endereço do cliente (balde grosseiro) | — | Opt-in, normalmente ausente; uma página carrega vários assets |

As operações de negócio pagam **os dois** limites: o grosseiro por endereço e o
por credencial. O grosseiro é mais largo por padrão, de modo que a credencial
seja a quota efetiva de um integrador e uma chave ruidosa não esgote primeiro o
balde compartilhado com outra chave atrás do mesmo proxy. O limite por origem
continua sendo a barreira contra tráfego distribuído entre credenciais.

Três decisões merecem justificativa explícita.

**Health tem balde próprio.** Se compartilhasse o balde grosseiro, tráfego
comum de um endereço poderia fazer a probe daquele endereço falhar, e uma
implantação saudável seria retirada de rotação por carga. Ainda assim ele é
limitado, porque uma rota anônima sem teto continua sendo um alvo.

**O webhook é global e generoso.** O IP da Stripe não é identidade que se possa
acreditar nem prever; por isso o webhook não paga também o balde grosseiro por
endereço. Seu balde global roda na mesma camada externa, antes da leitura do
corpo e da assinatura: contar por endereço permitiria que um peer forjado
escapasse do limite e, pior, recusaria uma reentrega legítima vinda de um
endereço novo. A confiança real continua sendo a assinatura sobre o corpo bruto.

**Operações são limitadas como escrita de negócio.** `reprocess` reenfileira
trabalho: é uma escrita com custo assíncrono, não uma leitura barata.

Uma operação nova do contrato que ninguém classificar cai no default da classe
de negócio. Ela nasce limitada, e não fora da política por omissão.

### Memória: capacidade fixa, LRU e TTL

Toda chave de limitador vem, em última instância, de algo que o cliente envia.
Por isso nenhum mapa é ilimitado. Cada limitador tem:

- uma **capacidade** máxima de baldes, com evicção do menos recentemente usado;
- um **TTL de ociosidade**, que recupera baldes intocados mesmo abaixo da
  capacidade, para que um processo quieto encolha em vez de manter o pico.

A evicção erra numa direção só, e isso é intencional: perder um balde devolve
um burst novo à chave, nunca recusa quem deveria passar. Um balde cheio também
é indistinguível de um ausente, então descartar um ocioso não perde nenhuma
capacidade de enforcement. O `RATE_LIMIT_IDLE_TTL` é validado para não ser
menor que nenhum intervalo de refill, senão um balde seria recuperado enquanto
ainda estava se recompondo.

Os defaults dimensionam o limitador grosseiro em 10 000 baldes, alguns
milhares de bytes cada — centenas de kilobytes no pior caso, contra um processo
que já mantém um pool de conexões.

### Resposta: 429 com o envelope público do projeto

Uma recusa devolve `429` com o mesmo envelope de erro de qualquer outra falha:
`code`, `message` e `correlationId`, mais os headers de segurança estritos do
[ADR 0016](0016-http-surface-and-client-identity.md) e o `X-Correlation-ID`. O
código estável é `rate_limited`.

Acrescenta-se `Retry-After`, em segundos inteiros, arredondado para cima e
nunca menor que um: um cliente recusado precisa saber quando voltar, e
arredondar uma espera de meio segundo para zero convidaria a repetição
imediata.

A resposta **não diz qual limite foi atingido**. Contar que a recusa veio do
balde de credencial confirmaria, a quem chutou uma chave, que ela é válida.

O `429` foi declarado no OpenAPI nas seis operações efetivamente limitadas, e a
suíte contratual exige uma prova por status declarado, então cada uma tem um
caso que exercita a cadeia real de middlewares.

### Ordem na cadeia: antes do parsing, da autenticação e do handler

```text
correlation id
  -> security headers
  -> resolução do endereço do cliente
  -> LIMITE PRÉ-ROTA                <- cliente, health ou webhook global
  -> otelhttp
  -> access log / recovery / limite de corpo
  -> roteamento e decode OpenAPI
  -> LIMITADOR POR CREDENCIAL / WEBHOOK
  -> autenticação por operationId
  -> handler
  -> caso de uso
```

O limite pré-rota fica imediatamente dentro da resolução de endereço e fora de
todo o resto. Negócio, operações, documentação e caminhos desconhecidos gastam
o balde grosseiro do endereço; health gasta seu balde de endereço separado; o
webhook gasta diretamente o balde global do provedor. Uma requisição recusada
ali custa uma consulta em mapa: nada foi parseado, nenhuma credencial comparada,
nenhum handler entrado.

O limitador por credencial fica dentro do strict server, onde o `operationId`
existe, e imediatamente **antes** da autenticação. Isso não enfraquece a
autenticação: só uma credencial que o verificador já reconhece chega a um
balde, e uma inválida segue recusada com `401`.

### Observabilidade sem cardinalidade

Uma recusa incrementa `http.server.rate_limited`, com exatamente dois atributos:
`limiter` (`client`, `credential`, `webhook` ou `health`) e `route.class`
(`health`, `docs`, `business`, `operations` ou `webhook`). Ambos são
vocabulários fechados,
validados pelo allowlist do [ADR 0015](0015-application-metrics-and-cardinality.md).

Endereço, impressão da credencial, chave de API e caminho HTTP nunca aparecem
em métrica, log ou trace. O número de séries é fixado por essas duas listas, e
não pelo número de chamadores.

### Comportamento por processo

Os limites são **por processo**. Cada réplica mantém os próprios baldes em
memória, sem coordenação e sem estado compartilhado.

## Alternativas consideradas

**Estado compartilhado em Redis ou PostgreSQL.** Daria limites globais exatos
entre réplicas. Rejeitado para a versão `0.1`: Redis seria uma dependência de
infraestrutura nova só para isso, e PostgreSQL colocaria uma escrita no caminho
de toda requisição, inclusive das que o limitador existe para descartar barato.
Um limitador que precisa de I/O para recusar tráfego hostil é um amplificador.

**Delegar tudo ao proxy ou à plataforma.** É a resposta certa para volumetria
alta, e continua recomendada como camada adicional. Não substitui esta, porque
o projeto é self-hosted e clonado: não há garantia de que exista um proxy na
frente, e o default precisa ser seguro sem ele. `RATE_LIMIT_ENABLED=false`
existe justamente para quem já resolveu isso na borda.

**Chavear o limitador autenticado pela API key em texto puro.** Rejeitado sem
discussão: colocaria o segredo numa chave de mapa, de onde ele vaza em dump de
heap, log de depuração e mensagem de erro.

**Contar o webhook por IP de origem.** Trataria o endereço da Stripe como
identidade, que é justamente o que o [ADR 0016](0016-http-surface-and-client-identity.md)
recusa fazer em geral.

**Janela fixa por simplicidade.** Permite o dobro do limite na virada da
janela, e o burst é a única coisa que o limite realmente precisa controlar.

**Mapa sem limite com limpeza periódica.** É o padrão mais comum e o mais
frágil: entre duas limpezas, o mapa cresce com o que o cliente enviar.

## Consequências

### Positivas

- Rotas anônimas deixam de ter custo ilimitado, e a recusa acontece antes de
  qualquer trabalho caro.
- Um laço acidental no backend do integrador é contido antes de esgotar a cota
  da Stripe da instalação.
- Uma credencial inválida não cria estado no processo.
- O consumo de memória do controle é limitado por configuração, não pelo
  tráfego.
- A identidade do cliente é a mesma que o resto da aplicação já usa, então
  proxy e rate limiting não podem discordar sobre quem chamou.

### Limitações

- **Múltiplas réplicas multiplicam o limite.** Com N réplicas atrás de um
  balanceador, o teto efetivo é N vezes o configurado, e um cliente cujas
  requisições caem em réplicas diferentes é contado separadamente em cada uma.
  Quem precisa de um teto global exato deve limitar na borda ou dividir os
  valores pelo número de réplicas, aceitando que uma réplica fora do ar aperta
  o limite para todos.
- **Reinício zera os baldes.** Um deploy devolve um burst novo a todo mundo.
  É aceitável porque a janela é de um burst, não de uma janela inteira.
- **Evicção sob pressão afrouxa o limite.** Mais chamadores distintos ativos do
  que a capacidade configurada faz baldes serem descartados e recriados cheios.
  A capacidade precisa ser dimensionada acima da população real de chamadores;
  o default de 10 000 endereços cobre com folga uma instalação típica.
- **Um proxy mal configurado concentra todo o tráfego num balde.** Sem
  `TRUSTED_PROXY_CIDRS`, todas as requisições vindas de um proxy são atribuídas
  ao endereço dele e compartilham um único balde grosseiro. É a consequência
  já registrada no [ADR 0016](0016-http-surface-and-client-identity.md), agora
  com efeito visível.
- **O limite não distingue custo entre operações da mesma classe.** Uma leitura
  de pedido e um checkout consomem um token cada, embora o segundo seja bem mais
  caro. Uma ponderação por custo exigiria uma decisão nova.

## Referências

- [OWASP API4:2023 Unrestricted Resource Consumption](https://owasp.org/API-Security/editions/2023/en/0xa4-unrestricted-resource-consumption/)
- [RFC 9110 §10.2.3 — Retry-After](https://www.rfc-editor.org/rfc/rfc9110#field.retry-after)
- [RFC 6585 §4 — 429 Too Many Requests](https://www.rfc-editor.org/rfc/rfc6585#section-4)
- [Stripe — melhores práticas de webhooks](https://docs.stripe.com/webhooks#best-practices)
