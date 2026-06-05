import os
import sys
import time
import subprocess

def get_tmux_target():
    out = subprocess.check_output("tmux list-sessions", shell=True).decode('utf-8')
    for line in out.splitlines():
        if "polymarket-indexing" in line:
            name = line.split(":")[0]
            return f"{name}:0"
    raise RuntimeError("Could not find tmux session containing 'polymarket-indexing'")

def run_benchmark(target, run_id, version):
    is_v1 = (version == "v1")
    log_path = f"tmp/profiles/{run_id}_{version}.log"
    os.makedirs("tmp/profiles", exist_ok=True)
    if os.path.exists(log_path):
        os.remove(log_path)
    
    args = f"--cpuprofile tmp/profiles/{run_id}_{version}.cpu.pprof"
    if is_v1:
        args += " --v1"
        
    cmd = f'POLYMARKET_ARGS="{args}" make dev > "{log_path}" 2>&1'
    print(f"Starting {version.upper()} in tmux target {target}...")
    
    subprocess.check_call(f"tmux send-keys -t {target} '{cmd}' C-m", shell=True)
    
    print(f"Waiting for {version.upper()} to start indexing (cursor mode)...")
    start_time = time.time()
    started = False
    
    while time.time() - start_time < 300:
        if os.path.exists(log_path):
            with open(log_path, 'r') as f:
                content = f.read()
            if "starting from block" in content and "cursor mode" in content:
                print(f"{version.upper()} started indexing. Running measurement for 45s...")
                started = True
                break
        time.sleep(1)
        
    if not started:
        print(f"Error: {version.upper()} did not start indexing in time.")
        sys.exit(1)
        
    time.sleep(45)
    
    print(f"Stopping {version.upper()} with SIGINT (Ctrl+C)...")
    subprocess.check_call(f"tmux send-keys -t {target} C-c", shell=True)
    
    print(f"Waiting for {version.upper()} to write profile and exit...")
    stop_time = time.time()
    finished = False
    while time.time() - stop_time < 60:
        with open(log_path, 'r') as f:
            content = f.read()
        if "Done." in content or "cpu profile written" in content:
            print(f"{version.upper()} completed successfully.")
            finished = True
            break
        time.sleep(1)
        
    if not finished:
        print(f"Warning: {version.upper()} did not report completion/profile write in log.")

def extract_metrics(run_id):
    v1_log = f"tmp/profiles/{run_id}_v1.log"
    v2_log = f"tmp/profiles/{run_id}_v2.log"
    
    print("\nExtracting logs...")
    subprocess.run(f"rg 'LOAD STATE|starting from block|PROFILE|FETCH:|PARSE:|DECODE:|INSERT:|CUSTOM:|TOTAL:|Throughput:|Mem Alloc:|cpu profile written' tmp/profiles/{run_id}_v*.log", shell=True)
    
    print("\nRunning go tool pprof...")
    subprocess.run(f"go tool pprof -top -nodecount=20 tmp/profiles/{run_id}_v1.cpu.pprof > tmp/profiles/{run_id}_v1.pprof_top.txt", shell=True)
    subprocess.run(f"go tool pprof -top -nodecount=20 tmp/profiles/{run_id}_v2.cpu.pprof > tmp/profiles/{run_id}_v2.pprof_top.txt", shell=True)
    
    print("\nDone extracting. Performance logs and top lists generated.")

def main():
    target = get_tmux_target()
    print(f"Found target tmux session/pane: {target}")
    run_id = time.strftime("%Y%m%d_%H%M%S")
    print(f"RUN_ID = {run_id}")
    
    run_benchmark(target, run_id, "v1")
    time.sleep(5)
    run_benchmark(target, run_id, "v2")
    
    extract_metrics(run_id)
    
if __name__ == '__main__':
    main()
