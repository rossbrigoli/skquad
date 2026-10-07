{{- define "skquad.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "skquad.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "skquad.name" . -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "skquad.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "skquad.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "skquad.selectorLabels" -}}
app.kubernetes.io/name: {{ include "skquad.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "skquad.operatorName" -}}
{{- printf "%s-operator" (include "skquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "skquad.operatorImage" -}}
{{- printf "%s:%s" .Values.image.operator.repository .Values.image.operator.tag -}}
{{- end -}}

{{- define "skquad.apiServerName" -}}
{{- printf "%s-api-server" (include "skquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "skquad.apiServerImage" -}}
{{- printf "%s:%s" .Values.image.apiServer.repository .Values.image.apiServer.tag -}}
{{- end -}}

{{- define "skquad.llmGatewayName" -}}
{{- printf "%s-llm-gateway" (include "skquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "skquad.llmGatewayImage" -}}
{{- printf "%s:%s" .Values.image.llmGateway.repository .Values.image.llmGateway.tag -}}
{{- end -}}

{{- define "skquad.llmGatewayMasterKeySecretName" -}}
{{- if .Values.llmGateway.masterKeySecret.name -}}
{{- .Values.llmGateway.masterKeySecret.name -}}
{{- else -}}
{{- printf "%s-master-key" (include "skquad.llmGatewayName" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "skquad.gatewayInternalTokenSecretName" -}}
{{- if .Values.toolGateway.internalTokenSecret.name -}}
{{- .Values.toolGateway.internalTokenSecret.name -}}
{{- else -}}
{{- printf "%s-internal-token" (include "skquad.toolGatewayName" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "skquad.webName" -}}
{{- printf "%s-web" (include "skquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "skquad.webImage" -}}
{{- printf "%s:%s" .Values.image.web.repository .Values.image.web.tag -}}
{{- end -}}

{{- define "skquad.postgresName" -}}
{{- printf "%s-postgres" (include "skquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "skquad.postgresSecretName" -}}
{{- if .Values.postgres.existingSecret -}}
{{- .Values.postgres.existingSecret -}}
{{- else -}}
{{- include "skquad.postgresName" . -}}
{{- end -}}
{{- end -}}

{{- define "skquad.embedderName" -}}
{{- printf "%s-embedder" (include "skquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "skquad.embedderImage" -}}
{{- printf "%s:%s" .Values.embedder.image.repository .Values.embedder.image.tag -}}
{{- end -}}

{{- define "skquad.toolGatewayName" -}}
{{- printf "%s-tool-gateway" (include "skquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "skquad.toolGatewayImage" -}}
{{- printf "%s:%s" .Values.image.toolGateway.repository .Values.image.toolGateway.tag -}}
{{- end -}}

{{- /* TG-6: browser service quarantine zone helpers */ -}}
{{- define "skquad.browserServiceName" -}}
{{- printf "%s-browser-service" (include "skquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "skquad.browserServiceImage" -}}
{{- printf "%s:%s" .Values.browserService.image.repository .Values.browserService.image.tag -}}
{{- end -}}

{{- define "skquad.browserProxyImage" -}}
{{- printf "%s:%s" .Values.browserService.proxyImage.repository .Values.browserService.proxyImage.tag -}}
{{- end -}}

{{- define "skquad.browserInternalTokenSecretName" -}}
{{- if .Values.browserService.internalTokenSecret.name -}}
{{- .Values.browserService.internalTokenSecret.name -}}
{{- else -}}
{{- printf "%s-internal-token" (include "skquad.browserServiceName" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- /* TG-10: terminal-service (Terminal-as-a-Service) helpers */ -}}
{{- define "skquad.terminalServiceName" -}}
{{- printf "%s-terminal-service" (include "skquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "skquad.terminalServiceImage" -}}
{{- printf "%s:%s" .Values.terminalService.image.repository .Values.terminalService.image.tag -}}
{{- end -}}

{{- define "skquad.stepCAName" -}}
{{- printf "%s-step-ca" (include "skquad.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "skquad.stepCAImage" -}}
{{- printf "%s:%s" .Values.terminalService.stepCA.image.repository .Values.terminalService.stepCA.image.tag -}}
{{- end -}}

{{- define "skquad.terminalInternalTokenSecretName" -}}
{{- if .Values.terminalService.internalTokenSecret.name -}}
{{- .Values.terminalService.internalTokenSecret.name -}}
{{- else -}}
{{- printf "%s-internal-token" (include "skquad.terminalServiceName" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
