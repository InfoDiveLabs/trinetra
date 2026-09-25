package trinetra

import "testing"

func TestParseSmartAttrsHDD(t *testing.T) {
	s := `smartctl 7.2 2020-12-30 r5155 [x86_64-linux-5.10.0] (local build)
Copyright (C) 2002-20, Bruce Allen, Christian Franke, www.smartmontools.org

=== START OF READ SMART DATA SECTION ===
SMART Attributes Data Structure revision number: 16
Vendor Specific SMART Attributes with Thresholds:
ID# ATTRIBUTE_NAME          FLAG     VALUE WORST THRESH TYPE      UPDATED  WHEN_FAILED RAW_VALUE
  1 Raw_Read_Error_Rate     0x000f   100   253   006    Pre-fail  Always       -       0
  5 Reallocated_Sector_Ct   0x0033   100   100   010    Pre-fail  Always       -       3
  9 Power_On_Hours          0x0032   100   100   000    Old_age   Always       -       12345
194 Temperature_Celsius     0x0022   109   095   000    Old_age   Always       -       35 (Min/Max 20/45)
`
	a := parseSmartAttrs(s)
	if a.TempC != 35 {
		t.Errorf("TempC = %d, want 35", a.TempC)
	}
	if a.ReallocSectors != 3 {
		t.Errorf("ReallocSectors = %d, want 3", a.ReallocSectors)
	}
	if a.WearPct != 0 {
		t.Errorf("WearPct = %d, want 0 (HDD has no wear attribute)", a.WearPct)
	}
}

func TestParseSmartAttrsSSD(t *testing.T) {
	s := `=== START OF READ SMART DATA SECTION ===
ID# ATTRIBUTE_NAME          FLAG     VALUE WORST THRESH TYPE      UPDATED  WHEN_FAILED RAW_VALUE
  5 Reallocated_Sector_Ct   0x0033   100   100   010    Pre-fail  Always       -       0
177 Wear_Leveling_Count     0x0013   094   094   000    Pre-fail  Always       -       120
190 Airflow_Temperature     0x0022   072   060   000    Old_age   Always       -       28
`
	a := parseSmartAttrs(s)
	if a.TempC != 28 {
		t.Errorf("TempC = %d, want 28", a.TempC)
	}
	if a.WearPct != 6 {
		t.Errorf("WearPct = %d, want 6 (100-VALUE=100-94)", a.WearPct)
	}
	if a.ReallocSectors != 0 {
		t.Errorf("ReallocSectors = %d, want 0", a.ReallocSectors)
	}
}

func TestParseSmartAttrsEmpty(t *testing.T) {
	a := parseSmartAttrs("smartctl 7.2\nno recognizable table here\n")
	if a != (SmartAttr{}) {
		t.Errorf("a = %+v, want zero value", a)
	}
}
