import pandas as pd
import matplotlib.pyplot as plt
import sys

# Default filename, can be overridden by command line argument
filename = 'simulation_output.csv'
if len(sys.argv) > 1:
    filename = sys.argv[1]

try:
    # 1. Load the CSV data
    print(f"Loading data from {filename}...")
    df = pd.read_csv(filename)
    
    # 2. Ensure 'Time' is our X-axis
    if 'Time' not in df.columns:
        print("Error: Could not find 'Time' column in the CSV.")
        sys.exit(1)
        
    df.set_index('Time', inplace=True)

    # 3. Create the plot
    plt.figure(figsize=(10, 6))
    
    # pandas automatically plots all remaining columns against the index
    df.plot(ax=plt.gca(), linewidth=2)

    # 4. Make it look professional
    plt.title('Transient Analysis Results', fontsize=16, fontweight='bold')
    plt.xlabel('Time (Seconds)', fontsize=12)
    plt.ylabel('Voltage (Volts)', fontsize=12)
    
    plt.grid(True, which='both', linestyle='--', alpha=0.7)
    plt.legend(title='Nodes', loc='upper right')
    plt.tight_layout()

    # 5. Display the graph
    print("Generating plot...")
    plt.show()

except FileNotFoundError:
    print(f"Error: {filename} not found. Did you run the Go engine first?")
except Exception as e:
    print(f"An unexpected error occurred: {e}")