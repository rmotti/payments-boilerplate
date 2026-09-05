# Política de suporte

## Estado atual

O Payments Boilerplate está em fase experimental e ainda não possui release
suportada para produção. Durante a série `v0.x`, somente a minor mais recente
recebe correções.

Exemplo, se `v0.3.x` for a versão atual:

| Série | Estado |
| --- | --- |
| `v0.3.x` | Suportada em melhor esforço |
| `v0.2.x` | Encerrada |
| `v0.1.x` | Encerrada |

Não existe SLA de resposta, correção ou disponibilidade. A manutenção ocorre em
regime de melhor esforço.

## Ambientes suportados

- As duas releases mais recentes de Go ainda mantidas pelo projeto Go.
- Versões de PostgreSQL e RabbitMQ declaradas na matriz de CI.
- Versão do SDK e da API do provedor fixada pelo projeto.
- Imagens, configurações e comandos documentados no repositório.

Versões exatas serão adicionadas à matriz quando o módulo Go e o ambiente de CI
forem criados. Uma versão não exercitada no CI não deve ser anunciada como
suportada.

## Canais

- Bugs reproduzíveis: GitHub Issues.
- Propostas e dúvidas: GitHub Discussions, quando habilitado.
- Vulnerabilidades: canal privado descrito em [SECURITY.md](SECURITY.md).

Antes de abrir um bug, reúna versão do projeto, sistema operacional, passos para
reprodução, comportamento esperado e logs sanitizados. Nunca publique secrets,
credenciais ou dados reais de pagamentos.

## Dentro do escopo

- Bugs reproduzíveis no código original.
- Vulnerabilidades do boilerplate.
- Erros no contrato OpenAPI ou na documentação.
- Incompatibilidade com versões declaradas na matriz.
- Falhas na topologia RabbitMQ fornecida pelo projeto.
- Violações das garantias documentadas de idempotência, inbox e outbox.

## Fora do escopo

- Forks e customizações que não possam reproduzir o problema no código original.
- Operação da infraestrutura particular de um integrador.
- Configuração de contas ou aprovação comercial no provedor.
- Problemas de adquirentes, bancos ou meios de pagamento.
- Consultoria PCI DSS, LGPD, fiscal ou jurídica.
- Resposta a incidentes financeiros de ambientes de terceiros.
- Backports para séries encerradas durante `v0.x`.

## Segurança

Relatos de vulnerabilidade não devem ser publicados em issues. A política e o
canal disponível estão em [SECURITY.md](SECURITY.md). Prazos de resposta de
segurança serão definidos antes da primeira release considerada estável.

## Evolução da política

Antes de `v1.0.0`, esta política será revisada para definir janela de manutenção,
depreciação, fim de vida e eventual suporte à major anterior. Nenhuma janela
futura será prometida antes de haver capacidade real de mantê-la.

