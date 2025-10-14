import argparse
import os
from kubernetes import client, config, watch
import time
import http.client
import redis


def timestamp_now():
    """
    Return current UTC timestamp in the same format as Go controller logs.
    Example: 2025-10-13T12:06:03.529Z
    """
    t = time.time()
    return time.strftime("%Y-%m-%dT%H:%M:%S.", time.gmtime(t)) + f"{int((t % 1) * 1000):03d}Z"

def check_application_readiness(app_url, app_port):
    """
    Check if the application is accessible using http.client.

    :param app_url: The application URL (IP address or hostname).
    :param app_port: The application port.
    :return: True if the application is accessible, False otherwise.
    """
    try:
        print(f"[DEBUG] Attempting to access {app_url}:{app_port}...")
        timeout_ms = 100 / 1000
        conn = http.client.HTTPConnection(app_url, app_port, timeout=timeout_ms)
        #conn = http.client.HTTPConnection(app_url, app_port)
        conn.request("GET", "/")
        response = conn.getresponse()
        print(f"[DEBUG] HTTP Status: {response.status}, Headers: {response.getheaders()}")
        if response.status == 200:
            return True
        return False
    except Exception as e:
        print(f"[DEBUG] Error accessing application: {e}")
        return False
    finally:
        conn.close()
def check_redis_readiness(app_url, app_port):
    """
    Check if the Redis server is accessible.

    :param app_url: The Redis server hostname or IP.
    :param app_port: The Redis server port.
    :return: True if the Redis server is accessible, False otherwise.
    """
    try:
        print(f"[DEBUG] Attempting to connect to Redis at {app_url}:{app_port}...")
        timeout_ms = 100 / 1000
        client = redis.Redis(host=app_url, port=app_port, socket_timeout=timeout_ms)
        return client.ping()
    except Exception as e:
        print(f"[DEBUG] Error accessing Redis: {e}")
        return False



def monitor_pod_events(namespace, label_selector, app_url, app_port,  app_type):
    """
    Monitor for new pods added or recreated during node draining and measure time metrics.

    :param namespace: Namespace where the pods reside.
    :param label_selector: Label selector to identify the pods.
    :param app_url: Application URL or IP address.
    :param app_port: Application port.
    """
    v1 = client.CoreV1Api()
    readiness_check = check_application_readiness if app_type == "http" else check_redis_readiness
    while True:
        try:
            w = watch.Watch()
            start_time = None
            pod_ready_time = None
            app_ready_time = None
            old_pod_name = None

            print("Monitoring pod events...")

            for event in w.stream(v1.list_namespaced_pod, namespace=namespace, label_selector=label_selector):
                pod = event['object']
                event_type = event['type']
                pod_phase = pod.status.phase
                pod_name = pod.metadata.name

                print(f"[DEBUG] Event: {event_type}, Pod: {pod_name}, Phase: {pod_phase}")

                # Handle new pod creation (kubectl apply)
                if event_type == "ADDED" and pod_phase == "Pending":
                    print("kubectl")
                    if start_time is None:  # Only measure the first pod in a sequence
                        start_time = time.time() * 1000
                        old_pod_name = pod_name
                        print(f"[{time.strftime('%H:%M:%S')}] New Pod {pod_name} detected as Pending...")

                # Handle pod recreation during migration (kubectl drain)
                if old_pod_name and pod_name != old_pod_name and event_type == "ADDED" and pod_phase == "Pending":
                    print("drain")
                    start_time = time.time() * 1000
                    print(f"[{time.strftime('%H:%M:%S')}] Recreated Pod {pod_name} detected as Pending...")

                # Measure time to pod ready
                if event_type == "MODIFIED" and pod_phase == "Running" and start_time is not None:
                    if pod_ready_time is None:  # Measure the first transition to Running
                        pod_ready_time = time.time() * 1000
                        node_name = pod.spec.node_name
                        print(f"[{time.strftime('%H:%M:%S')}] Pod {pod_name} is now Running on node {node_name}.")

                # Stop watching once the pod is Running and app is accessible
                if pod_ready_time and event_type == "MODIFIED" and pod_phase == "Running":
                    print("Waiting for application to become accessible...")
                    start_check_time = time.time() * 1000
                    max_wait_time = 600000  # 10 minutes in milliseconds

                    while app_ready_time is None and (time.time() * 1000 - start_check_time) < max_wait_time:
                        if readiness_check(app_url, app_port):
                            app_ready_time = time.time() * 1000
                            app_ready_timestamp = timestamp_now()
                            #print(f"Service Recovery Completed At: {app_ready_timestamp}")
                            print(f"[{time.strftime('%H:%M:%S')}] Application is accessible!")
                            break
                        #time.sleep(1)

                    break

            w.stop()

            if start_time and pod_ready_time and app_ready_time:
                print("\n=== Pod Event Timing Metrics ===")
                print(f"Time to Pod Ready: {pod_ready_time - start_time:.2f} ms")
                print(f"Time to Application Ready: {app_ready_time - start_check_time:.2f} ms")
                print(f"Total Recovery time: {app_ready_time - start_time:.2f}")
                print(f"Service Recovery Completed At: {app_ready_timestamp}")
                print("================================")
            else:
                print("Metrics incomplete: Some events did not occur.")

            print("Resuming monitoring for new pod events...")
        except Exception as e:
            print(f"[ERROR] {e}")
            #time.sleep(1)  # Avoid tight retry loops on errors

# Monitor for new pods and migrations

#monitor_pod_events(
#    namespace="default",
#    label_selector="app=video",
#    app_url="52.221.201.236",
#    app_port=30080,    #redis:31625, mongo:30257
#    app_type="http"  # Use "http" for HTTP-based apps or "redis" for Redis
#)



# ========== Entry Point ==========

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Kubernetes Application Recovery Measurement Tool")
    parser.add_argument("--kubeconfig", type=str, default="~/.kube/config",
                        help="Path to kubeconfig file (default: ~/.kube/config)")
    parser.add_argument("--namespace", type=str, default="default",
                        help="Kubernetes namespace to monitor (default: default)")
    parser.add_argument("--label", type=str, default="app=video",
                        help="Label selector for target pods (default: app=video)")
    parser.add_argument("--app-url", type=str, required=True,
                        help="Application endpoint IP or hostname")
    parser.add_argument("--app-port", type=int, required=True,
                        help="Application service port")
    parser.add_argument("--app-type", type=str, choices=["http", "redis"], default="http",
                        help="Type of application for readiness check (default: http)")
    args = parser.parse_args()

    kubeconfig_path = os.path.abspath(os.path.expanduser(args.kubeconfig))
    config.load_kube_config(config_file=kubeconfig_path)

    print(f"[INFO] Loaded kubeconfig from {kubeconfig_path}")
    print(f"[INFO] Monitoring namespace={args.namespace}, label={args.label}, app={args.app_url}:{args.app_port}")

    monitor_pod_events(
        namespace=args.namespace,
        label_selector=args.label,
        app_url=args.app_url,
        app_port=args.app_port,
        app_type=args.app_type,
    )

