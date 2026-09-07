# ADR 0010: modelo de acesso às rotas

- Status: aceito
- Data: 2026-09-06
- Implementação: pendente

## Contexto

O projeto é uma API headless self-hosted: cada pessoa ou equipe clona o
repositório, configura sua própria infraestrutura e integra o backend do seu
sistema. O consumidor final abre o Stripe Checkout hospedado, mas não chama
diretamente a Payments API.

Mesmo sem receber PAN ou CVV, a API cria pedidos, inicia operações no provedor e
expõe estado financeiro. Essas operações não podem ficar disponíveis a clientes
anônimos. Ao mesmo tempo, health checks e webhooks possuem atores e mecanismos
de confiança diferentes do backend integrador.

O modelo de acesso precisa ser definido antes do middleware para evitar uma
regra única que bloqueie probes ou entregas da Stripe, deixe novas operações
públicas por acidente ou introduza multi-tenancy fora do escopo.

## Decisão

### Limite da implantação

A versão `0.1` adota um integrador por implantação. Cada instalação possui seu
próprio PostgreSQL, credenciais Stripe e credenciais de acesso. Uma integração
autenticada pode operar todos os pedidos daquela instalação.

Não serão adicionados `tenant_id`, organizações, usuários ou autorização por
consumidor. Uma instalação compartilhada entre integradores independentes é um
modelo SaaS multi-tenant e permanece fora do escopo.

### Autenticação do integrador

As rotas de negócio exigirão uma chave opaca no header `X-API-Key`. As chaves
serão fornecidas por `INTEGRATION_API_KEYS`, com suporte a mais de uma chave
ativa para permitir rotação sem indisponibilidade.

As chaves devem:

- ter pelo menos 256 bits de entropia e ser geradas por fonte criptográfica;
- existir explicitamente em todos os ambientes, sem segredo padrão embutido;
- trafegar apenas em headers e, fora de loopback local, sobre HTTPS;
- nunca aparecer em URLs, respostas, logs, traces ou métricas;
- ser comparadas de forma resistente a ataques de timing.

Chave ausente ou inválida produzirá a mesma resposta `401 Unauthorized`, com
código estável `unauthorized` e sem revelar a causa. `403 Forbidden` fica
reservado para uma autorização futura por escopo, papel ou tenant.

### Política das rotas

A política será protegida por padrão. Somente operações presentes em uma lista
pública explícita poderão ignorar a chave do integrador.

| Operação | Acesso na versão 0.1 |
| --- | --- |
| `POST /v1/orders` | `X-API-Key` obrigatória |
| `POST /v1/orders/{orderId}/checkout` | `X-API-Key` obrigatória |
| `GET /v1/orders/{orderId}` | `X-API-Key` obrigatória |
| `POST /v1/webhooks/stripe` | Público na rede; `Stripe-Signature` obrigatória |
| `GET /health` da API | Público, com resposta mínima |
| `GET /docs` e `GET /openapi.yaml` | Disponíveis em desenvolvimento; desabilitados por padrão em produção |
| `GET /health` do worker | Restrito à rede privada da infraestrutura |

Público não significa confiável. O webhook é alcançável pela Stripe, mas só é
aceito depois da verificação criptográfica do header `Stripe-Signature` sobre o
corpo bruto. O identificador opaco do pedido é defesa adicional contra
enumeração, não substituto para autenticação ou autorização.

### Fronteira arquitetural

A autenticação será aplicada pela camada HTTP antes do handler de negócio. Com
o strict server atual, o fluxo será:

```text
correlation id
  -> limite do corpo
  -> roteamento e decode OpenAPI
  -> middleware de autenticacao por operationId
  -> handler
  -> caso de uso
```

O limite de 64 KiB continua aplicado antes do decode. O middleware usará o
`operationId` gerado pelo OpenAPI para manter uma lista pública pequena e negar
por padrão as demais operações.

Validação de credenciais pertence à infraestrutura HTTP. Domínio, application
services, catálogo e repositórios não recebem headers nem conhecem API keys.

O OpenAPI declarará um `securityScheme` do tipo `apiKey` quando o middleware for
implementado. Até lá, o contrato executável não anunciará uma proteção que o
runtime ainda não aplica.

## Alternativas consideradas

### Manter todas as rotas públicas

Rejeitada porque permite abuso de criação e checkout e expõe estado de pedidos.
IDs aleatórios não constituem controle de acesso.

### Autenticação de consumidores

Rejeitada no MVP. O consumidor interage com o produto do integrador e com o
checkout hospedado, não diretamente com esta API.

### OAuth 2.0, OpenID Connect ou mTLS obrigatórios

Adiados como padrão por exigirem um provedor de identidade ou gestão de
certificados, contrariando a experiência pequena e self-hosted do projeto. São
alternativas recomendadas para adoções com requisitos maiores; a API key não é
apresentada como solução universal para sistemas críticos.

### Multi-tenancy na mesma implantação

Rejeitado por exigir identidade estável do integrador em pedidos e consultas,
gestão de organizações, isolamento de dados e possivelmente Stripe Connect.
Essa evolução exige uma nova decisão de produto e arquitetura.

## Consequências

### Positivas

- O modelo acompanha a forma como o repositório será clonado e implantado.
- A proteção não contamina o domínio com detalhes de HTTP.
- Novas operações nascem protegidas até serem deliberadamente liberadas.
- Rotação pode ocorrer mantendo duas chaves ativas por um período curto.
- Swagger continua útil localmente sem ampliar a superfície de produção.

### Limitações

- Uma chave comprometida dá acesso a todos os pedidos da instalação.
- Não existe isolamento entre várias empresas no mesmo banco.
- Revogação e rotação dependem de configuração e novo deploy.
- Quem adotar o projeto continua responsável por TLS, gestão de secrets,
  controles de borda e adequação do mecanismo ao seu risco.

## Estado de implementação

Esta decisão documenta a política, mas não altera o comportamento atual. O item
seguinte do roadmap implementará configuração, verificador, middleware, erros,
OpenAPI e testes. Até essa entrega, as rotas já existentes continuam sem
autenticação de integração.

## Referências

- [OWASP REST Security Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/REST_Security_Cheat_Sheet.html)
- [OWASP API1:2023 Broken Object Level Authorization](https://owasp.org/API-Security/editions/2023/en/0xa1-broken-object-level-authorization/)
- [OpenAPI 3.1 — Security Scheme Object](https://spec.openapis.org/oas/v3.1.0#security-scheme-object)
- [Stripe — verificação de assinatura de webhooks](https://docs.stripe.com/webhooks/signature)
