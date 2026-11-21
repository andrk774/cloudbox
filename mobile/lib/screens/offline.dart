import 'package:flutter/material.dart';
import 'package:open_filex/open_filex.dart';

import '../api.dart';
import '../offline.dart';

class OfflineScreen extends StatefulWidget {
  final OfflineStore store;
  const OfflineScreen({super.key, required this.store});
  @override
  State<OfflineScreen> createState() => _OfflineScreenState();
}

class _OfflineScreenState extends State<OfflineScreen> {
  List<OfflineEntry>? _items;
  String? _error;
  @override
  void initState() {
    super.initState();
    _load();
  }

  Future<void> _load() async {
    try {
      final items = await widget.store.list();
      if (mounted) {
        setState(() {
          _items = items;
          _error = null;
        });
      }
    } catch (_) {
      if (mounted) {
        setState(() => _error = 'Не удалось прочитать сохранённые файлы');
      }
    }
  }

  @override
  Widget build(BuildContext context) => Scaffold(
    appBar: AppBar(title: const Text('Без интернета')),
    body: _error != null
        ? Center(child: Text(_error!))
        : _items == null
        ? const Center(child: CircularProgressIndicator())
        : _items!.isEmpty
        ? const Center(
            child: Padding(
              padding: EdgeInsets.all(24),
              child: Text(
                'Сохранённых файлов пока нет. В меню файла выберите «Сохранить без интернета».',
                textAlign: TextAlign.center,
              ),
            ),
          )
        : ListView(
            children: [
              const Padding(
                padding: EdgeInsets.all(16),
                child: Text(
                  'Здесь сохранённые копии. Для обновления скачайте файл заново. При выходе из аккаунта копии удаляются.',
                ),
              ),
              for (final item in _items!)
                ListTile(
                  leading: const Icon(Icons.offline_pin),
                  title: Text(item.name),
                  subtitle: Text(formatSize(item.size)),
                  onTap: () async {
                    final result = await OpenFilex.open(item.path);
                    if (!context.mounted || result.type == ResultType.done) {
                      return;
                    }
                    ScaffoldMessenger.of(context)
                        .showSnackBar(SnackBar(content: Text(result.message)));
                  },
                  trailing: IconButton(
                    tooltip: 'Удалить с устройства',
                    icon: const Icon(Icons.delete_outline),
                    onPressed: () async {
                      try {
                        await widget.store.remove(item.id);
                        await _load();
                      } catch (_) {
                        if (mounted) {
                          setState(() => _error = 'Не удалось удалить копию');
                        }
                      }
                    },
                  ),
                ),
            ],
          ),
  );
}
