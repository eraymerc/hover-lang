import pandas as pd
import numpy as np
import matplotlib.pyplot as plt
from matplotlib.backends.backend_tkagg import FigureCanvasTkAgg, NavigationToolbar2Tk
import sys
import os
import json
import tkinter as tk
from tkinter import ttk, messagebox

# --- 1. Configuration & Data Loading ---
filename = 'simulation_output.csv'
if len(sys.argv) > 1:
    filename = sys.argv[1]

try:
    print(f"Loading data from {filename}...")
    df = pd.read_csv(filename)
    if 'Time' not in df.columns:
        print("Error: Could not find 'Time' column in the CSV.")
        sys.exit(1)
        
    df.set_index('Time', inplace=True)
    available_columns = df.columns.tolist()
    
    if not available_columns:
        print("Error: No data columns found besides 'Time'.")
        sys.exit(1)

except FileNotFoundError:
    print(f"Error: {filename} not found. Did you run the engine first?")
    sys.exit(1)
except Exception as e:
    print(f"An unexpected error occurred: {e}")
    sys.exit(1)

# --- 2. Macro Engine Setup ---
JSON_FILE = 'custom_functions.json'

# This dictionary is the safe sandbox where custom functions run.
custom_namespace = {
    'pd': pd,
    'np': np,
    'df': df
}
# Inject all CSV columns into the namespace as variables (e.g., allows typing "va" instead of df["va"])
for col in df.columns:
    custom_namespace[col] = df[col]

def load_custom_functions():
    """Loads functions from JSON and compiles them into memory."""
    if not os.path.exists(JSON_FILE):
        return []
    try:
        with open(JSON_FILE, 'r') as f:
            funcs = json.load(f)
        for name, code in funcs.items():
            exec(code, custom_namespace)
        return list(funcs.keys())
    except Exception as e:
        print(f"Error loading functions: {e}")
        return []

# --- 3. Build Main GUI ---
root = tk.Tk()
root.title("Interactive Transient Analysis Viewer")
root.geometry("1150x750")

# Layout frames
control_frame = tk.Frame(root, width=250, padx=10, pady=10)
control_frame.pack(side=tk.LEFT, fill=tk.Y)

plot_frame = tk.Frame(root)
plot_frame.pack(side=tk.RIGHT, fill=tk.BOTH, expand=True)

# Plot setup
fig, ax = plt.subplots(figsize=(8, 5))
fig.tight_layout(pad=3.0)

canvas = FigureCanvasTkAgg(fig, master=plot_frame)
canvas_widget = canvas.get_tk_widget()
canvas_widget.pack(side=tk.TOP, fill=tk.BOTH, expand=True)

toolbar = NavigationToolbar2Tk(canvas, plot_frame)
toolbar.update()
toolbar.pack(side=tk.BOTTOM, fill=tk.X)

# --- 4. Plotting Logic ---
def update_plot():
    ax.clear()
    selected_columns = [col for col, var in checkbox_vars.items() if var.get()]
    
    if selected_columns:
        df[selected_columns].plot(ax=ax, linewidth=2)
        ax.set_title('Transient Analysis Results', fontsize=16, fontweight='bold')
        ax.set_xlabel('Time (Seconds)', fontsize=12)
        ax.set_ylabel('Amplitude', fontsize=12)
        ax.grid(True, which='both', linestyle='--', alpha=0.7)
        ax.legend(title='Variables', loc='upper right')
    else:
        ax.set_title('No variables selected', fontsize=16)
        ax.grid(True, which='both', linestyle='--', alpha=0.7)

    canvas.draw()

# --- 5. Dynamic Checkbox Controls ---
tk.Label(control_frame, text="Standard Variables:", font=('Arial', 11, 'bold')).pack(anchor='w')

checkbox_frame = tk.Frame(control_frame)
checkbox_frame.pack(fill=tk.X, pady=5)
checkbox_vars = {}

def add_checkbox(col_name, default_state=True):
    var = tk.BooleanVar(value=default_state)
    chk = tk.Checkbutton(checkbox_frame, text=col_name, variable=var, font=('Arial', 10), command=update_plot)
    chk.pack(anchor='w')
    checkbox_vars[col_name] = var

for col in available_columns:
    add_checkbox(col)

def toggle_all():
    any_unchecked = any(not var.get() for var in checkbox_vars.values())
    for var in checkbox_vars.values():
        var.set(any_unchecked)
    update_plot()

tk.Button(control_frame, text="Toggle All", command=toggle_all).pack(anchor='w', pady=(0, 10))
ttk.Separator(control_frame, orient='horizontal').pack(fill='x', pady=10)

# --- 6. Custom Function Tools (Macro Engine) ---
tk.Label(control_frame, text="Custom Functions:", font=('Arial', 11, 'bold')).pack(anchor='w')

# Dropdown to select function
func_combo = ttk.Combobox(control_frame, values=load_custom_functions(), state="readonly")
func_combo.pack(fill='x', pady=5)
if func_combo['values']:
    func_combo.current(0)

# Input arguments entry
tk.Label(control_frame, text="Inputs (e.g. va, vb, 0.5):", font=('Arial', 9)).pack(anchor='w')
args_entry = tk.Entry(control_frame)
args_entry.pack(fill='x', pady=5)

def execute_function():
    func_name = func_combo.get()
    args = args_entry.get()
    
    if not func_name:
        messagebox.showwarning("Warning", "Select a function first.")
        return
        
    command = f"{func_name}({args})"
    try:
        # Run the function
        result = eval(command, custom_namespace)
        
        # Determine how to display the result
        if isinstance(result, pd.Series):
            # It's a curve! Add it to the dataframe and create a checkbox
            new_col_name = f"{func_name}_out"
            df[new_col_name] = result
            custom_namespace[new_col_name] = result # Add to namespace for future use
            
            if new_col_name not in checkbox_vars:
                add_checkbox(new_col_name, default_state=True)
            else:
                checkbox_vars[new_col_name].set(True)
            
            update_plot()
            messagebox.showinfo("Success", f"Curve '{new_col_name}' added and plotted!")
        else:
            # It's a scalar value (like THD or RMS)
            messagebox.showinfo(f"{func_name} Result", f"Output:\n{result}")
            
    except Exception as e:
        messagebox.showerror("Execution Error", f"Failed to run '{command}':\n{e}")

tk.Button(control_frame, text="Run Function", command=execute_function, bg='#4CAF50', fg='white').pack(fill='x', pady=5)

# --- 7. Custom Function Editor Window ---
def open_editor(existing_code=None):
    editor = tk.Toplevel(root)
    editor.title("Function Editor")
    editor.geometry("500x400")
    
    tk.Label(editor, text="Write your Python function:", font=('Arial', 10, 'bold')).pack(anchor='w', padx=10, pady=(10,0))
    tk.Label(editor, text="Available packages: np, pd. Available variables: df, va, vb, etc.", font=('Arial', 8)).pack(anchor='w', padx=10)
    
    text_box = tk.Text(editor, font=("Consolas", 11), wrap="none")
    text_box.pack(fill="both", expand=True, padx=10, pady=10)
    
    # If editing an existing function, load its code. Otherwise, show the template.
    if existing_code:
        text_box.insert("1.0", existing_code)
    else:
        text_box.insert("1.0", "def my_func(curve1, curve2):\n    # Return a single number OR a new curve\n    return curve1 * curve2")
    
    def save_and_compile():
        code = text_box.get("1.0", tk.END).strip()
        if not code.startswith("def "):
            messagebox.showerror("Error", "Code must start with 'def function_name(...):'")
            return
            
        try:
            # Extract function name
            first_line = code.split('\n')[0]
            func_name = first_line.split('def ')[1].split('(')[0].strip()
            
            # Save to JSON
            funcs = {}
            if os.path.exists(JSON_FILE):
                with open(JSON_FILE, 'r') as f:
                    funcs = json.load(f)
            
            funcs[func_name] = code
            with open(JSON_FILE, 'w') as f:
                json.dump(funcs, f, indent=4)
                
            # Compile into memory
            exec(code, custom_namespace)
            
            # Update GUI dropdown
            func_combo['values'] = list(funcs.keys())
            func_combo.set(func_name)
            
            messagebox.showinfo("Success", f"Function '{func_name}' saved and compiled!")
            editor.destroy()
            
        except Exception as e:
            messagebox.showerror("Compile Error", f"Error saving function:\n{e}")

    tk.Button(editor, text="Save & Compile", command=save_and_compile, bg='#2196F3', fg='white').pack(pady=(0,10))


def edit_selected_function():
    func_name = func_combo.get()
    if not func_name:
        messagebox.showwarning("Warning", "Select a function from the dropdown to edit.")
        return
        
    try:
        with open(JSON_FILE, 'r') as f:
            funcs = json.load(f)
            if func_name in funcs:
                open_editor(existing_code=funcs[func_name])
            else:
                messagebox.showerror("Error", "Function code not found in JSON.")
    except Exception as e:
        messagebox.showerror("Error", f"Could not open function:\n{e}")

# --- Layout for Editor Buttons ---
ttk.Separator(control_frame, orient='horizontal').pack(fill='x', pady=10)

editor_btn_frame = tk.Frame(control_frame)
editor_btn_frame.pack(fill='x')

tk.Button(editor_btn_frame, text="✏️ Edit", command=edit_selected_function).pack(side=tk.LEFT, fill='x', expand=True, padx=(0, 2))
tk.Button(editor_btn_frame, text="➕ New", command=lambda: open_editor()).pack(side=tk.RIGHT, fill='x', expand=True, padx=(2, 0))

# --- 8. Start Application ---
update_plot()
root.mainloop()