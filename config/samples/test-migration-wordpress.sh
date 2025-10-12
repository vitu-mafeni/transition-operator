#!/bin/bash

# ----------- USER CONFIGURATION ------------
DEPLOYMENT_NAME="edge-wordpress"
NAMESPACE="wordpress"
APP_LABEL="wordpress"
NODEPORT=31269
SERVICE_PATH="/"

CLUSTER_A_IP="192.168.28.250"
CLUSTER_B_IP="192.168.28.131"
KUBECONFIG_A="../../cluster1vt.kubeconfig"
KUBECONFIG_B="../../cluster2vt.kubeconfig"
TIMEOUT=1

# ----------- LOG SETUP ------------
LOG_DIR="./migration_logs"
mkdir -p "$LOG_DIR"

log() {
  echo "[$(date +'%F %T')] $1" | tee -a "$LOG_FILE"
}

log_pod_status() {
  local cluster_name=$1
  local kubeconfig=$2
  local phase=$3

  log "[$cluster_name] --- POD STATUS SNAPSHOT during $phase ---"
  kubectl --kubeconfig="$kubeconfig" get pods -n "$NAMESPACE" -l app="$APP_LABEL" -o wide | tee -a "$LOG_FILE"
}

# ----------- INFINITE MONITORING LOOP ------------

LOG_DIR="./migration_logs"
mkdir -p "$LOG_DIR"
LOG_FILE="$LOG_DIR/wordpress_migration_log_$(date +%Y%m%d_%H%M%S).log"

while true; do
  # LOG_FILE="$LOG_DIR/wordpress_migration_log_$(date +%Y%m%d_%H%M%S).log"
  declare -A events
  for cluster in A B; do
    events["deploy_$cluster"]=0
    events["running_$cluster"]=0
    events["ready_$cluster"]=0
    events["service_$cluster"]=0
  done

  log ""
  log "=== NEW MIGRATION MONITORING CYCLE STARTED ==="
  log "Tracking deployment: $DEPLOYMENT_NAME in namespace: $NAMESPACE"

  while true; do
    NOW=$(date +%s)
    URL_A="http://${CLUSTER_A_IP}:${NODEPORT}${SERVICE_PATH}"
    URL_B="http://${CLUSTER_B_IP}:${NODEPORT}${SERVICE_PATH}"

    # ---- Cluster A Checks ----
    if [ "${events[deploy_A]}" -eq 0 ]; then
      if kubectl --kubeconfig="$KUBECONFIG_A" get deployment "$DEPLOYMENT_NAME" -n "$NAMESPACE" &>/dev/null; then
        events[deploy_A]=$NOW
        log "[A] Deployment detected at $(date -d @$NOW)"
      fi
    fi

    if [ "${events[running_A]}" -eq 0 ]; then
      RUNNING_POD=$(kubectl --kubeconfig="$KUBECONFIG_A" get pods -n "$NAMESPACE" -l app="$APP_LABEL" -o json | \
        jq -r '.items[] | select(.metadata.deletionTimestamp == null) | select(.status.phase == "Running") | .metadata.name' | head -n 1)
      if [[ -n "$RUNNING_POD" ]]; then
        events[running_A]=$NOW
        log "[A] Pod is Running ($RUNNING_POD) at $(date -d @$NOW)"
        log_pod_status "A" "$KUBECONFIG_A" "Running"
      fi
    fi

    if [ "${events[ready_A]}" -eq 0 ]; then
      READY_POD=$(kubectl --kubeconfig="$KUBECONFIG_A" get pods -n "$NAMESPACE" -l app="$APP_LABEL" -o json | \
        jq -r '.items[] | select(.metadata.deletionTimestamp == null) | select(.status.phase == "Running") | select(.status.conditions[] | select(.type=="Ready" and .status=="True")) | .metadata.name' | head -n 1)
      if [[ -n "$READY_POD" ]]; then
        events[ready_A]=$NOW
        log "[A] Pod is Ready ($READY_POD) at $(date -d @$NOW)"
        log_pod_status "A" "$KUBECONFIG_A" "Ready"
      fi
    fi

    if [ "${events[service_A]}" -eq 0 ]; then
      CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time $TIMEOUT "$URL_A")
      if [ "$CODE" == "200" ]; then
        events[service_A]=$NOW
        log "[A] Service AVAILABLE at $(date -d @$NOW)"
      fi
    fi

    # ---- Cluster B Checks ----
    if [ "${events[deploy_B]}" -eq 0 ]; then
      if kubectl --kubeconfig="$KUBECONFIG_B" get deployment "$DEPLOYMENT_NAME" -n "$NAMESPACE" &>/dev/null; then
        events[deploy_B]=$NOW
        log "[B] Deployment detected at $(date -d @$NOW)"
      fi
    fi

    if [ "${events[running_B]}" -eq 0 ]; then
      RUNNING_POD=$(kubectl --kubeconfig="$KUBECONFIG_B" get pods -n "$NAMESPACE" -l app="$APP_LABEL" -o json | \
        jq -r '.items[] | select(.metadata.deletionTimestamp == null) | select(.status.phase == "Running") | .metadata.name' | head -n 1)
      if [[ -n "$RUNNING_POD" ]]; then
        events[running_B]=$NOW
        log "[B] Pod is Running ($RUNNING_POD) at $(date -d @$NOW)"
        log_pod_status "B" "$KUBECONFIG_B" "Running"
      fi
    fi

    if [ "${events[ready_B]}" -eq 0 ]; then
      READY_POD=$(kubectl --kubeconfig="$KUBECONFIG_B" get pods -n "$NAMESPACE" -l app="$APP_LABEL" -o json | \
        jq -r '.items[] | select(.metadata.deletionTimestamp == null) | select(.status.phase == "Running") | select(.status.conditions[] | select(.type=="Ready" and .status=="True")) | .metadata.name' | head -n 1)
      if [[ -n "$READY_POD" ]]; then
        events[ready_B]=$NOW
        log "[B] Pod is Ready ($READY_POD) at $(date -d @$NOW)"
        log_pod_status "B" "$KUBECONFIG_B" "Ready"
      fi
    fi

    if [ "${events[service_B]}" -eq 0 ]; then
      CODE=$(curl -s -o /dev/null -w "%{http_code}" --max-time $TIMEOUT "$URL_B")
      if [ "$CODE" == "200" ]; then
        events[service_B]=$NOW
        log "[B] Service AVAILABLE at $(date -d @$NOW)"
        break
      fi
    fi

    sleep 1
  done

  # ----------- SUMMARY REPORT ------------
  log "--------- MIGRATION SUMMARY ---------"
  for cluster in A B; do
    for phase in deploy running ready service; do
      TS=${events["${phase}_$cluster"]}
      [ "$TS" -ne 0 ] && log "[$cluster] ${phase^}: $(date -d @$TS)"
    done
  done

  # Downtime calculation
  if [ "${events[service_A]}" -ne 0 ] && [ "${events[service_B]}" -ne 0 ]; then
    downtime=$((events[service_B] - events[service_A]))
    log "Total downtime between A and B service: $downtime seconds"
  fi

  log "Migration monitoring cycle completed."
  log "Waiting 1 second before restarting monitoring..."
  log "---------------------------------------------"
  echo ""
  echo ""
  sleep 1
done
