{{/* The release name is the service name: Deployment, Service and ServiceAccount are all called this. */}}
{{- define "service.name" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Every pod of the release (server, PreSync Job, CronJobs): the NetworkPolicy selects these. */}}
{{- define "service.selectorLabels" -}}
app.kubernetes.io/name: {{ include "service.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Only the long-running pods: Deployment, Service, PDB and spread constraints select these, never Job pods. */}}
{{- define "service.serverSelectorLabels" -}}
{{ include "service.selectorLabels" . }}
app.kubernetes.io/component: server
{{- end -}}

{{- define "service.labels" -}}
{{ include "service.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | quote }}
app.kubernetes.io/part-of: tasuki
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* Fail early, at render time, instead of shipping something the cluster will reject or kill. */}}
{{- define "service.validate" -}}
{{- if not .Values.image.repository -}}
{{- fail "image.repository is required" -}}
{{- end -}}
{{- if or (not .Values.image.tag) (eq (toString .Values.image.tag) "latest") -}}
{{- fail "image.tag is required and must not be \"latest\"" -}}
{{- end -}}
{{- $budget := add .Values.preStopSleepSeconds .Values.shutdownDrainSeconds -}}
{{- if gt (int $budget) (int .Values.terminationGracePeriodSeconds) -}}
{{- fail (printf "preStopSleepSeconds + shutdownDrainSeconds (%d s) exceeds terminationGracePeriodSeconds (%d s): the kubelet would SIGKILL mid-drain" (int $budget) (int .Values.terminationGracePeriodSeconds)) -}}
{{- end -}}
{{- if and .Values.rollout.enabled (gt (int .Values.rollout.rollbackWindowRevisions) (int .Values.revisionHistoryLimit)) -}}
{{- fail "rollout.rollbackWindowRevisions exceeds revisionHistoryLimit: the ReplicaSets to roll back to would already be deleted" -}}
{{- end -}}
{{- end -}}

{{- define "service.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end -}}

{{/*
Environment variables: secrets first, then plain values, each sorted so renders are stable.
The OpenTelemetry defaults are merged under .Values.env, so a service can override any of them
(and no name appears twice, which server-side apply would reject).
*/}}
{{- define "service.env" -}}
{{- $env := deepCopy .Values.env -}}
{{- with .Values.otlp.endpoint -}}
{{- $env = merge $env (dict
      "OTEL_EXPORTER_OTLP_ENDPOINT" .
      "OTEL_SERVICE_NAME" (include "service.name" $)
      "OTEL_RESOURCE_ATTRIBUTES" (printf "deployment.environment.name=%s,service.version=%s" $.Release.Namespace (toString $.Values.image.tag))) -}}
{{- end -}}
{{- range $name := keys .Values.secretEnv | sortAlpha }}
{{- $ref := index $.Values.secretEnv $name }}
- name: {{ $name }}
  valueFrom:
    secretKeyRef:
      name: {{ required (printf "secretEnv.%s.secret is required" $name) $ref.secret }}
      key: {{ required (printf "secretEnv.%s.key is required" $name) $ref.key }}
{{- end }}
{{- range $name := keys $env | sortAlpha }}
- name: {{ $name }}
  value: {{ index $env $name | toString | quote }}
{{- end }}
{{- end -}}

{{/*
Pod spec shared by the Deployment (or Rollout), the PreSync Job and CronJobs.
Call with a dict: root (the chart context), args (overrides .Values.args when set),
long (true for the long-running pods: probes, preStop, the drain budget and the files volume),
serviceAccount (overrides the release's own ServiceAccount).
*/}}
{{- define "service.podSpec" -}}
{{- $root := .root -}}
{{- $v := $root.Values -}}
serviceAccountName: {{ .serviceAccount | default (include "service.name" $root) }}
automountServiceAccountToken: {{ $v.serviceAccount.automountToken }}
enableServiceLinks: false
{{- with $v.priorityClassName }}
priorityClassName: {{ . }}
{{- end }}
securityContext:
  {{- toYaml $v.podSecurityContext | nindent 2 }}
{{- if .long }}
terminationGracePeriodSeconds: {{ $v.terminationGracePeriodSeconds }}
{{- else }}
restartPolicy: Never
{{- end }}
containers:
  - name: app
    image: {{ include "service.image" $root }}
    imagePullPolicy: {{ $v.image.pullPolicy }}
    {{- with $v.command }}
    command:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    {{- with (.args | default $v.args) }}
    args:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    {{- if .long }}
    ports:
      - name: http
        containerPort: {{ $v.port }}
        protocol: TCP
      {{- if $v.metrics.port }}
      - name: metrics
        containerPort: {{ $v.metrics.port }}
        protocol: TCP
      {{- end }}
    {{- end }}
    {{- with (include "service.env" $root | trim) }}
    env:
      {{- . | nindent 6 }}
    {{- end }}
    {{- if .long }}
    {{- with $v.probes.startup }}
    startupProbe:
      {{- toYaml . | nindent 6 }}
    {{- end }}
    livenessProbe:
      {{- toYaml (required "probes.liveness is required" $v.probes.liveness) | nindent 6 }}
    readinessProbe:
      {{- toYaml (required "probes.readiness is required" $v.probes.readiness) | nindent 6 }}
    lifecycle:
      preStop:
        # Built-in sleep action (no shell needed in distroless images): keep serving while the endpoint
        # is removed from Services and the ALB, then SIGTERM starts the app's own drain.
        sleep:
          seconds: {{ $v.preStopSleepSeconds }}
    {{- end }}
    resources:
      {{- toYaml $v.resources | nindent 6 }}
    securityContext:
      {{- toYaml $v.securityContext | nindent 6 }}
    volumeMounts:
      - name: tmp
        mountPath: /tmp
      {{- if and .long $v.files }}
      - name: files
        mountPath: {{ $v.filesMountPath }}
        readOnly: true
      {{- end }}
volumes:
  - name: tmp
    emptyDir:
      sizeLimit: {{ $v.tmpVolume.sizeLimit }}
  {{- if and .long $v.files }}
  - name: files
    configMap:
      name: {{ include "service.name" $root }}-files
  {{- end }}
{{- with $v.nodeSelector }}
nodeSelector:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $v.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $v.affinity }}
affinity:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}

{{/*
Pod template of the long-running pods, shared by the Deployment and the Rollout. Call with a dict: root (the chart
context) and hashLabel, the label the controller adds per revision (Deployment: pod-template-hash, Argo Rollouts:
rollouts-pod-template-hash), so the spread constraints count only pods of the same revision.
*/}}
{{- define "service.serverPodTemplate" -}}
{{- $root := .root -}}
{{- $v := $root.Values -}}
{{- $annotations := deepCopy $v.podAnnotations -}}
{{- with $v.files -}}
{{- /* A changed file changes the pod template, so the pods roll and read it (a ConfigMap update alone does not). */ -}}
{{- $_ := set $annotations "checksum/files" (toJson . | sha256sum) -}}
{{- end -}}
metadata:
  labels:
    {{- include "service.labels" $root | nindent 4 }}
    app.kubernetes.io/component: server
    {{- with $v.podLabels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  {{- with $annotations }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
spec:
  {{- include "service.podSpec" (dict "root" $root "long" true) | nindent 2 }}
  {{- if $v.topologySpread.enabled }}
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: {{ $v.topologySpread.zoneWhenUnsatisfiable }}
      labelSelector:
        matchLabels:
          {{- include "service.serverSelectorLabels" $root | nindent 10 }}
      # Count only pods of the same revision, so a rollout is spread on its own terms.
      matchLabelKeys: [{{ .hashLabel }}]
    - maxSkew: 1
      topologyKey: kubernetes.io/hostname
      whenUnsatisfiable: {{ $v.topologySpread.hostWhenUnsatisfiable }}
      labelSelector:
        matchLabels:
          {{- include "service.serverSelectorLabels" $root | nindent 10 }}
      matchLabelKeys: [{{ .hashLabel }}]
  {{- end }}
{{- end -}}
