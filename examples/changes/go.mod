module github.com/aoiflux/libvhdi/examples/changes

go 1.26.2

require (
	github.com/aoiflux/libvhdi v0.0.0
	github.com/aoiflux/libvhdi/change v0.0.0
)

require (
	github.com/aoiflux/libext v0.3.0 // indirect
	github.com/aoiflux/libfat v0.3.1 // indirect
	github.com/aoiflux/libhfs v0.3.2 // indirect
	github.com/aoiflux/libntfs v0.3.3 // indirect
	github.com/aoiflux/libtable v0.2.2 // indirect
	github.com/aoiflux/libxfat v1.4.0 // indirect
	github.com/aoiflux/libxfs v0.4.1 // indirect
)

replace github.com/aoiflux/libvhdi => ../..

replace github.com/aoiflux/libvhdi/change => ../../change
